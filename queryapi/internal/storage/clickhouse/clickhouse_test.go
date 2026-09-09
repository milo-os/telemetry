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
		"SELECT ObservedTimestamp, Body, ServiceName, SeverityText, TraceId, ResourceAttributes, LogAttributes FROM logs",
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

func TestBuildLogsQueryAttributeMatcherUsesMap(t *testing.T) {
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
	if !strings.Contains(query, "ResourceAttributes[?] = ?") {
		t.Errorf("resource matcher did not use a map access: %s", query)
	}
	if len(args) != 6 {
		t.Errorf("len(args) = %d, want 6 (project,range) + key + value", len(args))
	}
	if args[3] != "resource_name" {
		t.Errorf("args[3] (map key) = %v, want resource_name", args[3])
	}
	if args[4] != "gateway-us-east" {
		t.Errorf("args[4] (matcher value) = %v, want gateway-us-east", args[4])
	}
}

func TestBuildLogsQueryRejectsNonPositiveLimit(t *testing.T) {
	q := parseQuery(t, `{service_name="a"}`)
	lq := storage.LogQuery{Matchers: q.Matchers, Range: tr(), Limit: 0}
	if _, _, err := buildLogsQuery("p", lq); !errors.Is(err, storage.ErrInvalidLimit) {
		t.Errorf("buildLogsQuery limit 0 = %v, want ErrInvalidLimit", err)
	}
}

func TestBuildLabelNamesQuery(t *testing.T) {
	query, args := buildLabelNamesQuery("proj-abc", tr())
	if strings.Count(query, "arrayJoin(mapKeys(") != 2 {
		t.Errorf("expected two mapKeys fragments:\n%s", query)
	}
	// Two fragments, each binding project+range = 6 placeholders.
	if strings.Count(query, "ProjectId = ?") != 2 {
		t.Errorf("expected project filter in both fragments:\n%s", query)
	}
	if len(args) != 6 {
		t.Errorf("len(args) = %d, want 6", len(args))
	}
	for i := 0; i < 6; i += 3 {
		if args[i] != "proj-abc" {
			t.Errorf("args[%d] = %v, want project", i, args[i])
		}
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
	if !strings.Contains(attrQuery, "SELECT DISTINCT LogAttributes[?] FROM logs") {
		t.Errorf("attribute label query incorrect: %s", attrQuery)
	}
	if attrArgs[0] != "http_method" {
		t.Errorf("attribute map key arg = %v, want http_method", attrArgs[0])
	}
	if attrArgs[1] != "p" {
		t.Errorf("attrArgs[1] = %v, want project", attrArgs[1])
	}
}

func TestBuildSeriesQueryGroupsAllowlist(t *testing.T) {
	q := parseQuery(t, `{service_name="envoy-gateway"}`)
	query, args := buildSeriesQuery("proj-abc", q.Matchers, tr())

	if !strings.Contains(query, "GROUP BY ServiceName, SeverityText, ResourceAttributes[?]") {
		t.Errorf("series query does not group the allowlist:\n%s", query)
	}
	if !strings.Contains(query, "ServiceName = ?") {
		t.Errorf("series query missing the selector matcher:\n%s", query)
	}
	// SELECT key + WHERE (project+range+value) + GROUP BY key = 6 args.
	if len(args) != 6 {
		t.Errorf("len(args) = %d, want 6 (got %v)", len(args), args)
	}
	if args[0] != "resource_name" || args[5] != "resource_name" {
		t.Errorf("args[0]=%v args[5]=%v, want resource_name bound for SELECT and GROUP BY", args[0], args[5])
	}
	if args[1] != "proj-abc" {
		t.Errorf("args[1] = %v, want project", args[1])
	}
}

func TestAssembleLabels(t *testing.T) {
	ls := assembleLabels("envoy-gateway", "INFO", "trace-1",
		map[string]string{"resource_name": "gateway-us-east", "host": ""},
		map[string]string{"http.method": "GET", "empty": ""})

	want := storage.LabelSet{
		"service_name":  "envoy-gateway",
		"severity":      "INFO",
		"trace_id":      "trace-1",
		"resource_name": "gateway-us-east",
		"http.method":   "GET",
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

// The sink writes an observed-time attribute on every record, so it would
// otherwise show up as a label on every row and in /labels.
func TestInternalAttributesAreNotLabels(t *testing.T) {
	ls := assembleLabels("envoy-gateway", "INFO", "",
		nil,
		map[string]string{"telemetry.observed_time_unix_nano": "1757340000000000000", "http.method": "GET"})

	if _, leaked := ls["telemetry.observed_time_unix_nano"]; leaked {
		t.Errorf("assembleLabels leaked the internal attribute: %v", ls)
	}
	if ls["http.method"] != "GET" {
		t.Errorf("assembleLabels dropped a real attribute: %v", ls)
	}

	got := withoutInternalAttributes([]string{"http.method", "telemetry.observed_time_unix_nano", "resource_name"})
	want := []string{"http.method", "resource_name"}
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
