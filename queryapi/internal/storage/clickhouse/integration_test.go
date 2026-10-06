// SPDX-License-Identifier: AGPL-3.0-only

package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"go.datum.net/o11y/queryapi/internal/logql"
	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// logsDDL mirrors config/clickhouse-migrations/migrations/000001_init.up.sql
// plus 000002_labels_column.up.sql's Labels column, so an integration test can
// build the production schema without reaching across module boundaries for
// the migration files. The store reads Labels, so a copy without it fails.
const logsDDL = `
CREATE TABLE logs
(
    Timestamp DateTime64(9),
    ObservedTimestamp DateTime64(9),
    TraceId String,
    SpanId String,
    TraceFlags UInt8,
    SeverityText LowCardinality(String),
    SeverityNumber UInt8,
    ServiceName LowCardinality(String),
    Body String,
    ResourceSchemaUrl LowCardinality(String),
    ResourceAttributes Map(String, String),
    ScopeSchemaUrl LowCardinality(String),
    ScopeName String,
    ScopeVersion LowCardinality(String),
    ScopeAttributes Map(String, String),
    LogAttributes Map(String, String),
    EventName String,
    ProjectId String MATERIALIZED ResourceAttributes['milo.project.id'],
    Labels Map(String, String) MATERIALIZED mapConcat(mapApply((k, v) -> (replaceAll(k, '.', '_'), v), LogAttributes), mapApply((k, v) -> (replaceAll(k, '.', '_'), v), ResourceAttributes))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ObservedTimestamp)
ORDER BY (ProjectId, ObservedTimestamp, ServiceName)`

// TestStoreAgainstClickHouse exercises the real LogQL-to-SQL translation
// against a live server. It skips unless CLICKHOUSE_TEST_ADDR is set (see
// `task queryapi:test-integration`), so plain `task test` stays offline.
func TestStoreAgainstClickHouse(t *testing.T) {
	addr := os.Getenv("CLICKHOUSE_TEST_ADDR")
	if addr == "" {
		t.Skip("CLICKHOUSE_TEST_ADDR not set")
	}

	const database = "o11y_queryapi_test"
	opts := &ch.Options{
		Addr: []string{addr},
		Auth: ch.Auth{
			Username: envOr("CLICKHOUSE_TEST_USER", "default"),
			Password: os.Getenv("CLICKHOUSE_TEST_PASSWORD"),
		},
	}

	boot, err := ch.Open(opts)
	if err != nil {
		t.Fatalf("open bootstrap connection: %v", err)
	}
	t.Cleanup(func() { _ = boot.Close() })
	if err := boot.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database); err != nil {
		t.Fatalf("drop database: %v", err)
	}
	if err := boot.Exec(context.Background(), "CREATE DATABASE "+database); err != nil {
		t.Fatalf("create database: %v", err)
	}

	// The store sends the per-query custom setting telemetry_project_id (see
	// projectContext). The server accepts it only because the container mounts
	// testdata/custom-settings.xml, which registers the telemetry_ prefix as
	// production's config does. No GRANT is involved.

	scoped := *opts
	scoped.Auth.Database = database
	conn, err := ch.Open(&scoped)
	if err != nil {
		t.Fatalf("open scoped connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.Exec(context.Background(), logsDDL); err != nil {
		t.Fatalf("create logs table: %v", err)
	}

	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	insertRows(t, conn, base)

	store := newStoreWithConn(conn)
	ctx := miloauth.WithProject(context.Background(), "proj-a")

	ran := storage.TimeRange{Start: base.Add(-time.Minute), End: base.Add(time.Hour)}
	if err := store.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	runQueryLogs(t, store, ctx, ran)
	runLabelNames(t, store, ctx, ran)
	runLabelValues(t, store, ctx, ran)
	runResourceOnlyLabelRoundTrip(t, store, ctx, ran)
	runSeries(t, store, ctx, ran)
	runSubsecondWindows(t, conn, store, base)

	// Tenancy holds even against a live server.
	if _, err := store.LabelNames(context.Background(), ran); err != storage.ErrNoProject {
		t.Errorf("LabelNames without a project = %v, want ErrNoProject", err)
	}
}

func insertRows(t *testing.T, conn driver.Conn, base time.Time) {
	t.Helper()
	// ObservedTimestamp is the query key, so it is written explicitly; the
	// test inserts observed == event time, which holds under normal conditions.
	const insert = "INSERT INTO logs (Timestamp, ObservedTimestamp, ServiceName, SeverityText, SeverityNumber, Body, ResourceAttributes, LogAttributes) VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
	for i, svc := range []string{"envoy-gateway", "envoy-gateway", "waf"} {
		sev := map[int]string{0: "INFO", 1: "ERROR", 2: "DEBUG"}[i]
		sevNum := map[int]uint8{0: 9, 1: 17, 2: 5}[i]
		if err := conn.Exec(context.Background(), insert,
			base.Add(time.Duration(i)*time.Second), base.Add(time.Duration(i)*time.Second), svc, sev, sevNum,
			"line "+string(rune('a'+i)),
			// k8s.node.name is written ONLY to the resource map, and dotted.
			// It is the shape that used to be advertised by /labels and then
			// read out of LogAttributes, resolving to nothing.
			map[string]string{"milo.project.id": "proj-a", "resource_name": "gateway-us-east", "k8s.node.name": "edge-3"},
			map[string]string{"http_method": "GET"},
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
}

// runSubsecondWindows checks that window bounds keep their sub-second part.
// Bound as time.Time, the driver formats them as toDateTime('<seconds>'), so a
// window inside one second matched the whole second or nothing (issue #197).
// The rows live in their own project so the other checks' counts hold.
func runSubsecondWindows(t *testing.T, conn driver.Conn, store *Store, base time.Time) {
	t.Helper()
	// The timestamps go in as integer nanoseconds: a time.Time bound here would
	// be rounded to the second just like the query bounds, and the windows
	// below would test nothing.
	const insert = "INSERT INTO logs (Timestamp, ObservedTimestamp, ServiceName, Body, ResourceAttributes) " +
		"VALUES (fromUnixTimestamp64Nano(?), fromUnixTimestamp64Nano(?), ?, ?, ?)"
	for _, ms := range []int{50, 150, 250} {
		at := base.Add(time.Duration(ms) * time.Millisecond).UnixNano()
		svc := fmt.Sprintf("at-%dms", ms)
		if err := conn.Exec(context.Background(), insert, at, at, svc, svc,
			map[string]string{"milo.project.id": "proj-sub"}); err != nil {
			t.Fatalf("insert %s: %v", svc, err)
		}
	}
	rows, err := conn.Query(context.Background(),
		"SELECT ObservedTimestamp FROM logs WHERE ProjectId = 'proj-sub' ORDER BY ObservedTimestamp")
	if err != nil {
		t.Fatalf("read back timestamps: %v", err)
	}
	var stored []time.Time
	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			t.Fatalf("read back timestamps: %v", err)
		}
		stored = append(stored, ts)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatalf("read back timestamps: %v", err)
	}
	for i, ms := range []int{50, 150, 250} {
		if want := base.Add(time.Duration(ms) * time.Millisecond); i >= len(stored) || !stored[i].Equal(want) {
			t.Fatalf("stored timestamps = %v, want %d ms past %v", stored, ms, base)
		}
	}
	ctx := miloauth.WithProject(context.Background(), "proj-sub")
	window := func(from, to int) storage.TimeRange {
		return storage.TimeRange{Start: base.Add(time.Duration(from) * time.Millisecond), End: base.Add(time.Duration(to) * time.Millisecond)}
	}

	for _, tt := range []struct {
		name     string
		from, to int
		want     []string
	}{
		{"inside one second", 100, 200, []string{"at-150ms"}},
		{"start inclusive, end exclusive", 150, 250, []string{"at-150ms"}},
		{"between rows", 160, 240, nil},
		{"whole second", 0, 1000, []string{"at-50ms", "at-150ms", "at-250ms"}},
	} {
		q := parse(t, `{service_name=~"at-.+"}`)
		iter, err := store.QueryLogs(ctx, storage.LogQuery{
			Matchers: q.Matchers, Range: window(tt.from, tt.to),
			Limit: 10, Direction: storage.DirectionForward,
		})
		if err != nil {
			t.Fatalf("QueryLogs %s: %v", tt.name, err)
		}
		var got []string
		for iter.Next() {
			got = append(got, iter.Row().Line)
		}
		if err := errors.Join(iter.Err(), iter.Close()); err != nil {
			t.Fatalf("QueryLogs %s: %v", tt.name, err)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("QueryLogs [+%dms, +%dms) = %v, want %v", tt.from, tt.to, got, tt.want)
		}

		// Label values share the same window clause.
		values, err := store.LabelValues(ctx, "service_name", window(tt.from, tt.to))
		if err != nil {
			t.Fatalf("LabelValues %s: %v", tt.name, err)
		}
		want := slices.Clone(tt.want)
		slices.Sort(want)
		slices.Sort(values)
		if !slices.Equal(values, want) {
			t.Errorf("LabelValues [+%dms, +%dms) = %v, want %v", tt.from, tt.to, values, want)
		}
	}

	// Bounds outside DateTime64(9)'s range are clamped to it; the server must
	// accept the clamped values and return everything.
	wide := storage.TimeRange{
		Start: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
	}
	values, err := store.LabelValues(ctx, "service_name", wide)
	if err != nil {
		t.Fatalf("LabelValues over years 1-9999: %v", err)
	}
	slices.Sort(values)
	if want := []string{"at-150ms", "at-250ms", "at-50ms"}; !slices.Equal(values, want) {
		t.Errorf("LabelValues over years 1-9999 = %v, want %v", values, want)
	}
}

func runQueryLogs(t *testing.T, store *Store, ctx context.Context, ran storage.TimeRange) {
	t.Helper()
	q := parse(t, `{service_name="envoy-gateway", severity=~"INFO|ERROR"} |= "line"`)
	iter, err := store.QueryLogs(ctx, storage.LogQuery{
		Matchers: q.Matchers, Filters: q.Filters, Range: ran,
		Limit: 10, Direction: storage.DirectionForward,
	})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}

	var rows []storage.Row
	for iter.Next() {
		rows = append(rows, iter.Row())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("QueryLogs iterate: %v", err)
	}
	if err := iter.Close(); err != nil {
		t.Fatalf("QueryLogs close: %v", err)
	}

	// Two envoy-gateway rows (INFO, ERROR); the waf DEBUG row is filtered out.
	if len(rows) != 2 {
		t.Fatalf("QueryLogs returned %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Labels["service_name"] != "envoy-gateway" {
			t.Errorf("row service_name = %q, want envoy-gateway", r.Labels["service_name"])
		}
		if r.Labels["resource_name"] != "gateway-us-east" {
			t.Errorf("row resource_name missing from labels: %v", r.Labels)
		}
		if r.Labels["http_method"] != "GET" {
			t.Errorf("row http_method = %q, want GET", r.Labels["http_method"])
		}
		if r.Line == "" {
			t.Error("row Line is empty")
		}
	}
}

func runLabelNames(t *testing.T, store *Store, ctx context.Context, ran storage.TimeRange) {
	t.Helper()
	names, err := store.LabelNames(ctx, ran)
	if err != nil {
		t.Fatalf("LabelNames: %v", err)
	}
	// Dotted OTel keys are advertised sanitized, which is the only spelling a
	// matcher accepts -- so the catalogue and the query path agree.
	for _, want := range []string{"service_name", "severity", "resource_name", "http_method", "milo_project_id", "k8s_node_name"} {
		if !containsStr(names, want) {
			t.Errorf("LabelNames missing %q, got %v", want, names)
		}
	}
	for _, unwanted := range []string{"milo.project.id", "k8s.node.name"} {
		if containsStr(names, unwanted) {
			t.Errorf("LabelNames advertised the unsanitized key %q, which no matcher accepts: %v", unwanted, names)
		}
	}
}

// runResourceOnlyLabelRoundTrip is the regression test for the defect this
// change fixes. Every label /labels advertises must be usable, so a
// resource-only dotted key has to survive all three paths: the catalogue, its
// values, and a matcher that actually selects rows.
func runResourceOnlyLabelRoundTrip(t *testing.T, store *Store, ctx context.Context, ran storage.TimeRange) {
	t.Helper()

	values, err := store.LabelValues(ctx, "k8s_node_name", ran)
	if err != nil {
		t.Fatalf("LabelValues(k8s_node_name): %v", err)
	}
	if !containsStr(values, "edge-3") {
		t.Errorf("LabelValues(k8s_node_name) = %v, want edge-3 -- a resource-only key resolved to nothing", values)
	}

	q := parse(t, `{k8s_node_name="edge-3"}`)
	iter, err := store.QueryLogs(ctx, storage.LogQuery{
		Matchers: q.Matchers, Range: ran, Limit: 10, Direction: storage.DirectionForward,
	})
	if err != nil {
		t.Fatalf("QueryLogs(k8s_node_name): %v", err)
	}
	var rows int
	for iter.Next() {
		if got := iter.Row().Labels["k8s_node_name"]; got != "edge-3" {
			t.Errorf("row k8s_node_name = %q, want edge-3: %v", got, iter.Row().Labels)
		}
		rows++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("QueryLogs(k8s_node_name) iterate: %v", err)
	}
	if err := iter.Close(); err != nil {
		t.Fatalf("QueryLogs(k8s_node_name) close: %v", err)
	}
	if rows != 3 {
		t.Errorf("matcher on a resource-only label selected %d rows, want 3", rows)
	}
}

func runLabelValues(t *testing.T, store *Store, ctx context.Context, ran storage.TimeRange) {
	t.Helper()
	svcValues, err := store.LabelValues(ctx, "service_name", ran)
	if err != nil {
		t.Fatalf("LabelValues(service_name): %v", err)
	}
	if !containsStr(svcValues, "envoy-gateway") || !containsStr(svcValues, "waf") {
		t.Errorf("LabelValues(service_name) = %v, want envoy-gateway and waf", svcValues)
	}

	resValues, err := store.LabelValues(ctx, "resource_name", ran)
	if err != nil {
		t.Fatalf("LabelValues(resource_name): %v", err)
	}
	if !containsStr(resValues, "gateway-us-east") {
		t.Errorf("LabelValues(resource_name) = %v, want gateway-us-east", resValues)
	}
}

func runSeries(t *testing.T, store *Store, ctx context.Context, ran storage.TimeRange) {
	t.Helper()
	q := parse(t, `{service_name="envoy-gateway"}`)
	series, err := store.Series(ctx, q.Matchers, ran)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	// Distinct bounded combos among envoy-gateway rows.
	if len(series) == 0 {
		t.Fatal("Series returned no label sets")
	}
	for _, ls := range series {
		if ls["service_name"] != "envoy-gateway" {
			t.Errorf("series service_name = %q, want envoy-gateway: %v", ls["service_name"], ls)
		}
	}
}

func parse(t *testing.T, raw string) *logql.Query {
	t.Helper()
	q, err := logql.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return q
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
