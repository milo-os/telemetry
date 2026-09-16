// SPDX-License-Identifier: AGPL-3.0-only

package clickhouse

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"go.datum.net/o11y/queryapi/internal/logql"
	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// stubConn satisfies driver.Conn by embedding it. Only Close is implemented:
// the methods under test (tenancy checks, query building) short-circuit before
// touching the connection, so any other call is a bug this test should surface
// as a nil-interface panic.
type stubConn struct {
	driver.Conn
}

func (stubConn) Close() error { return nil }

// captureConn records the last query and args passed to Query, letting store
// methods be exercised against a fake connection.
type captureConn struct {
	driver.Conn

	query string
	args  []any
}

func (c *captureConn) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	c.query = query
	c.args = args
	return nil, nil
}

func TestNoProjectBeatsQuery(t *testing.T) {
	store := newStoreWithConn(stubConn{})

	if _, err := store.QueryLogs(context.Background(), storage.LogQuery{Range: tr(), Limit: 1}); !errors.Is(err, storage.ErrNoProject) {
		t.Errorf("QueryLogs without a project = %v, want ErrNoProject", err)
	}
	if _, err := store.LabelNames(context.Background(), tr()); !errors.Is(err, storage.ErrNoProject) {
		t.Errorf("LabelNames without a project = %v, want ErrNoProject", err)
	}
	if _, err := store.LabelValues(context.Background(), "severity", tr()); !errors.Is(err, storage.ErrNoProject) {
		t.Errorf("LabelValues without a project = %v, want ErrNoProject", err)
	}
	if _, err := store.Series(context.Background(), nil, tr()); !errors.Is(err, storage.ErrNoProject) {
		t.Errorf("Series without a project = %v, want ErrNoProject", err)
	}
}

func TestQueryLogsPassesLimitToStore(t *testing.T) {
	conn := &captureConn{}
	store := newStoreWithConn(conn)
	ctx := miloauth.WithProject(context.Background(), "proj-abc")

	if _, err := store.QueryLogs(ctx, storage.LogQuery{Range: tr(), Limit: 0}); !errors.Is(err, storage.ErrInvalidLimit) {
		t.Errorf("QueryLogs with limit 0 = %v, want ErrInvalidLimit", err)
	}
	if _, err := store.QueryLogs(ctx, storage.LogQuery{Range: tr(), Limit: -1}); !errors.Is(err, storage.ErrInvalidLimit) {
		t.Errorf("QueryLogs with negative limit = %v, want ErrInvalidLimit", err)
	}
	if _, err := store.QueryLogs(ctx, storage.LogQuery{Range: tr(), Limit: 5, Direction: storage.DirectionBackward}); err != nil {
		t.Fatalf("QueryLogs with valid limit: %v", err)
	}
	if !strings.HasSuffix(conn.query, "LIMIT ?") {
		t.Errorf("query does not end in LIMIT ?: %s", conn.query)
	}
	last := conn.args[len(conn.args)-1]
	if last != 5 {
		t.Errorf("last bound arg = %v, want limit 5", last)
	}
}

func parseQuery(t *testing.T, raw string) *logql.Query {
	t.Helper()
	q, err := logql.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return q
}

func TestBuildLogsQueryTranslation(t *testing.T) {
	start := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	end := start.Add(time.Hour)
	tr := storage.TimeRange{Start: start, End: end}

	q := parseQuery(t, `{service_name="envoy-gateway", severity!="DEBUG"} |= "error" !~ "t[a-z]+"`)
	lq := storage.LogQuery{
		Matchers:  q.Matchers,
		Filters:   q.Filters,
		Range:     tr,
		Limit:     42,
		Direction: storage.DirectionBackward,
	}
	query, args, err := buildLogsQuery("proj-abc", lq)
	if err != nil {
		t.Fatalf("buildLogsQuery: %v", err)
	}

	for _, want := range []string{
		"SELECT " + logsSelect + " FROM logs",
		"ProjectId = ?",
		"ObservedTimestamp >= ?",
		"ObservedTimestamp < ?",
		"ServiceName = ?",
		"(SeverityText != ?)",
		"position(Body, ?) > 0",
		"NOT match(Body, ?)",
		"ORDER BY ObservedTimestamp DESC",
		"LIMIT ?",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q:\n%s", want, query)
		}
	}

	// Project first, then range, then the four matcher/filter values, limit last.
	wantLen := 3 + 2 + 2 + 1
	if len(args) != wantLen {
		t.Fatalf("len(args) = %d, want %d (got %v)", len(args), wantLen, args)
	}
	if args[0] != "proj-abc" {
		t.Errorf("args[0] = %v, want project", args[0])
	}
}

// An attribute matcher reads the merged, pre-normalised Labels column. The
// query no longer decides which map holds a key -- the schema already merged
// them -- so this is a single native lookup binding one parameter.
func TestBuildLogsQueryAttributeMatcherUsesLabelsColumn(t *testing.T) {
	q := parseQuery(t, `{resource_name="gateway-us-east"}`)
	lq := storage.LogQuery{
		Matchers:  q.Matchers,
		Range:     tr(),
		Limit:     10,
		Direction: storage.DirectionBackward,
	}
	query, args, err := buildLogsQuery("p", lq)
	if err != nil {
		t.Fatalf("buildLogsQuery: %v", err)
	}
	if !strings.Contains(query, "Labels[?] = ?") {
		t.Errorf("attribute matcher did not read the Labels column: %s", query)
	}
	for _, gone := range []string{"mapContains", "arrayFirst", "replaceAll"} {
		if strings.Contains(query, gone) {
			t.Errorf("matcher still references %q; normalisation belongs to the schema now: %s", gone, query)
		}
	}
	// project+range, the key, the value, the limit.
	if len(args) != 6 {
		t.Fatalf("len(args) = %d, want 6 (got %v)", len(args), args)
	}
	if args[3] != "resource_name" || args[4] != "gateway-us-east" {
		t.Errorf("args[3]=%v args[4]=%v, want key then value", args[3], args[4])
	}
}

// A dotted OTel key is spelled with underscores, and Resolve sanitises inbound
// so the bound key matches what the schema wrote.
func TestBuildLogsQueryMatcherBindsSanitizedKey(t *testing.T) {
	q := parseQuery(t, `{k8s_node_name="edge-3"}`)
	lq := storage.LogQuery{Matchers: q.Matchers, Range: tr(), Limit: 10}
	_, args, err := buildLogsQuery("p", lq)
	if err != nil {
		t.Fatalf("buildLogsQuery: %v", err)
	}
	if args[3] != "k8s_node_name" {
		t.Errorf("args[3] = %v, want the sanitized key k8s_node_name", args[3])
	}
}

// Rows are read as parallel key/value arrays rather than as a Map, because a
// sanitized-name collision leaves a duplicate key in Labels and the driver
// collapses a Map scan last-wins -- the opposite of the first-wins precedence
// SQL applies. Scanning arrays keeps both and lets assembleLabels match SQL.
func TestLogsSelectReadsLabelArrays(t *testing.T) {
	if !strings.Contains(logsSelect, "mapKeys(Labels)") || !strings.Contains(logsSelect, "mapValues(Labels)") {
		t.Errorf("logsSelect must read Labels as parallel arrays: %s", logsSelect)
	}
	if strings.Contains(logsSelect, "ResourceAttributes") || strings.Contains(logsSelect, "LogAttributes") {
		t.Errorf("logsSelect should no longer read the raw maps: %s", logsSelect)
	}
}

func TestBuildLogsQueryRejectsNonPositiveLimit(t *testing.T) {
	q := parseQuery(t, `{service_name="a"}`)
	lq := storage.LogQuery{Matchers: q.Matchers, Range: tr(), Limit: 0}
	if _, _, err := buildLogsQuery("p", lq); !errors.Is(err, storage.ErrInvalidLimit) {
		t.Errorf("buildLogsQuery limit 0 = %v, want ErrInvalidLimit", err)
	}
}

// The catalogue aggregates per window and drops any name that collided within
// a row, so /labels never advertises a name whose value silently lost half its
// provenance. Per-row filtering would let a row lacking the twin re-advertise
// a name another row suppressed.
func TestBuildLabelNamesQuerySuppressesCollisions(t *testing.T) {
	query, args := buildLabelNamesQuery("proj-abc", tr())

	for _, want := range []string{
		"mapKeys(Labels)",
		"countEqual(mapKeys(Labels), name) > 1",
		"GROUP BY name",
		"HAVING max(collided) = 0",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("label-names query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, "UNION ALL") {
		t.Errorf("label-names query should be a single aggregate:\n%s", query)
	}
	if len(args) != 3 {
		t.Fatalf("len(args) = %d, want 3 (project+range), got %v", len(args), args)
	}
	if args[0] != "proj-abc" {
		t.Errorf("args[0] = %v, want project", args[0])
	}
}

func TestBuildLabelValuesQueryColumnVsAttribute(t *testing.T) {
	colQuery, colArgs := buildLabelValuesQuery("p", "severity", tr())
	if !strings.Contains(colQuery, "SELECT DISTINCT SeverityText FROM logs") {
		t.Errorf("column label query incorrect: %s", colQuery)
	}
	if len(colArgs) != 3 {
		t.Errorf("column label args = %d, want 3", len(colArgs))
	}

	attrQuery, attrArgs := buildLabelValuesQuery("p", "http_method", tr())
	if !strings.Contains(attrQuery, "SELECT DISTINCT Labels[?] FROM logs") {
		t.Errorf("attribute label query incorrect: %s", attrQuery)
	}
	if attrArgs[0] != "http_method" {
		t.Errorf("attribute map key arg = %v, want http_method", attrArgs[0])
	}
	if attrArgs[1] != "p" {
		t.Errorf("attrArgs[1] = %v, want project", attrArgs[1])
	}
}

// The endpoint's original defect, at the query-builder level: a label whose
// key lives only in ResourceAttributes was advertised by /labels and then read
// from LogAttributes, so it returned nothing. Its values query must reach the
// resource map.
func TestBuildLabelValuesQueryReachesResourceOnlyKey(t *testing.T) {
	query, args := buildLabelValuesQuery("p", "k8s_node_name", tr())
	if !strings.Contains(query, "Labels[?]") {
		t.Errorf("resource-only label cannot resolve:\n%s", query)
	}
	if args[0] != "k8s_node_name" {
		t.Errorf("map key arg = %v, want k8s_node_name", args[0])
	}
}

func TestBuildSeriesQueryGroupsAllowlist(t *testing.T) {
	q := parseQuery(t, `{service_name="envoy-gateway"}`)
	query, args := buildSeriesQuery("proj-abc", q.Matchers, tr())

	if !strings.Contains(query, "GROUP BY ServiceName, SeverityText, Labels[?]") {
		t.Errorf("series query does not group the allowlist:\n%s", query)
	}
	if !strings.Contains(query, "ServiceName = ?") {
		t.Errorf("series query missing the selector matcher:\n%s", query)
	}
	// SELECT key + WHERE (project+range+value) + GROUP BY key, since the
	// GROUP BY text repeats the expression.
	wantLen := 1 + 4 + 1
	if len(args) != wantLen {
		t.Fatalf("len(args) = %d, want %d (got %v)", len(args), wantLen, args)
	}
	if args[0] != "resource_name" || args[len(args)-1] != "resource_name" {
		t.Errorf("args[0]=%v args[last]=%v, want resource_name bound for SELECT and GROUP BY",
			args[0], args[len(args)-1])
	}
	if args[1] != "proj-abc" {
		t.Errorf("args[1] = %v, want project", args[1])
	}
}

// Every label on a returned row must be one a matcher accepts, so keys are
// sanitized on the way out too. Returning http.method while the parser rejects
// that name is the round-trip break: the client is handed a dimension it
// cannot filter on.
func TestAssembleLabels(t *testing.T) {
	ls := assembleLabels("envoy-gateway", "INFO", "trace-1",
		[]string{"k8s_node_name", "host", "http_method", "empty"},
		[]string{"edge-3", "", "GET", ""})

	want := storage.LabelSet{
		"service_name":  "envoy-gateway",
		"severity":      "INFO",
		"trace_id":      "trace-1",
		"k8s_node_name": "edge-3",
		"http_method":   "GET",
	}
	if len(ls) != len(want) {
		t.Fatalf("assembleLabels = %v, want %v (empty attribute values must be dropped)", ls, want)
	}
	for k, v := range want {
		if ls[k] != v {
			t.Errorf("assembleLabels[%q] = %q, want %q", k, ls[k], v)
		}
	}
}

// TestAssembleLabelsShadowsCollidingSpellings pins what happens when two keys
// sanitize to one label name: the log attribute shadows the resource one, and
// the resource twin's value is not reachable under any name.
//
// This is a real limitation, not a nicety -- /labels advertises one dimension
// whose value comes from whichever twin a given row carries, so its meaning
// can differ row to row. It is pinned here so the precedence cannot change
// silently, and so the cost of the sanitization trade-off is visible in a test
// rather than only in a comment.
func TestAssembleLabelsShadowsCollidingSpellings(t *testing.T) {
	// Labels' own order, LogAttributes first per the 000002 migration.
	ls := assembleLabels("svc", "INFO", "",
		[]string{"k8s_pod_name", "k8s_pod_name"},
		[]string{"from-log", "from-resource"})

	if got := ls["k8s_pod_name"]; got != "from-log" {
		t.Errorf("k8s_pod_name = %q, want from-log (log attributes shadow resource ones)", got)
	}
	if got := ls["k8s_pod_name"]; got == "from-resource" {
		t.Errorf("the shadowed resource value won: %v", ls)
	}
	// One dimension, not two -- the collapse is what makes the value ambiguous.
	count := 0
	for k := range ls {
		if k == "k8s_pod_name" {
			count++
		}
	}
	if count != 1 || len(ls) != 3 {
		t.Errorf("colliding spellings did not collapse to one label: %v", ls)
	}
}

// A row carrying only the resource twin resolves the same label name to the
// resource value -- the other half of the ambiguity above.
func TestAssembleLabelsResourceTwinAloneResolves(t *testing.T) {
	ls := assembleLabels("svc", "INFO", "",
		[]string{"k8s_pod_name"}, []string{"from-resource"})

	if got := ls["k8s_pod_name"]; got != "from-resource" {
		t.Errorf("k8s_pod_name = %q, want from-resource", got)
	}
}

// The sink writes an observed-time attribute on every record, so it would
// otherwise show up as a label on every row and in /labels.
func TestInternalAttributesAreNotLabels(t *testing.T) {
	ls := assembleLabels("envoy-gateway", "INFO", "",
		[]string{"telemetry_observed_time_unix_nano", "http_method"},
		[]string{"1757340000000000000", "GET"})

	if _, leaked := ls["telemetry_observed_time_unix_nano"]; leaked {
		t.Errorf("assembleLabels leaked the internal attribute: %v", ls)
	}
	if ls["http_method"] != "GET" {
		t.Errorf("assembleLabels dropped a real attribute: %v", ls)
	}

	// The catalogue sanitizes keys in SQL, so this filter sees the sanitized
	// spelling. Comparing against the dotted key here would silently stop
	// suppressing it.
	got := withoutInternalAttributes([]string{"http_method", "telemetry_observed_time_unix_nano", "resource_name"})
	want := []string{"http_method", "resource_name"}
	if len(got) != len(want) {
		t.Fatalf("withoutInternalAttributes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("withoutInternalAttributes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func tr() storage.TimeRange {
	return storage.TimeRange{Start: time.Unix(100, 0), End: time.Unix(200, 0)}
}

// writeTestCerts writes a self-signed client certificate, its key, and a CA
// file (the same certificate) into a temp dir.
func writeTestCerts(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "queryapi"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(crand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	caFile = filepath.Join(dir, "ca.crt")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	for path, contents := range map[string][]byte{
		certFile: certPEM,
		keyFile:  keyPEM,
		caFile:   certPEM,
	} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return certFile, keyFile, caFile
}

// TestPingFailsWhenUnreachable exercises the real constructor, including TLS
// assembly, against a host that cannot resolve.
func TestPingFailsWhenUnreachable(t *testing.T) {
	certFile, keyFile, caFile := writeTestCerts(t)

	store, err := New(Config{
		Host: "clickhouse.invalid", Port: 9440, User: "queryapi", Database: "o11y",
		TLSCertFile: certFile, TLSKeyFile: keyFile, TLSCAFile: caFile,
	})
	if err != nil {
		t.Fatalf("New with generated certificates: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Logf("close store: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err == nil {
		t.Fatal("Ping to an unresolvable host succeeded, want error")
	}
}

// TestMigrationPutsLogAttributesFirst guards the one thing about this design
// that is silently wrong if reversed, and that no Go test would otherwise
// catch: precedence now lives in the schema, not in this package.
//
// mapConcat does not deduplicate and Map[k] returns the first match, so the
// argument order in 000002 decides which attribute wins a sanitized-name
// collision. assembleLabels applies first-wins to match it. Swap the arguments
// and SQL silently resolves to the resource attribute while this package still
// reports the log one -- the split between read path and query path that the
// original /labels defect was made of.
func TestMigrationPutsLogAttributesFirst(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..",
		"config", "clickhouse-migrations", "migrations", "000002_labels_column.up.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)

	// Anchor on the real statement: the comment above it also says
	// "ALTER TABLE logs MATERIALIZE COLUMN", and matching that would parse
	// prose instead of DDL.
	at := strings.Index(sql, "ALTER TABLE logs ADD COLUMN")
	if at < 0 {
		t.Fatalf("migration has no ADD COLUMN statement:\n%s", sql)
	}
	stmt := sql[at:]
	log := strings.Index(stmt, "LogAttributes")
	res := strings.Index(stmt, "ResourceAttributes")
	if log < 0 || res < 0 {
		t.Fatalf("migration does not merge both attribute maps:\n%s", stmt)
	}
	if log > res {
		t.Errorf("000002 lists ResourceAttributes before LogAttributes, so SQL resolves a "+
			"collision to the resource attribute while assembleLabels reports the log one:\n%s", stmt)
	}
	if !strings.Contains(stmt, "MATERIALIZED") {
		t.Errorf("Labels must be MATERIALIZED or the exporter's fixed INSERT list breaks:\n%s", stmt)
	}
	if !strings.Contains(stmt, "replaceAll(k, '.', '_')") {
		t.Errorf("migration does not sanitize dots to underscores:\n%s", stmt)
	}
}
