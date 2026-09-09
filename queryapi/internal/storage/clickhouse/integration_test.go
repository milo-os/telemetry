// SPDX-License-Identifier: AGPL-3.0-only

package clickhouse

import (
	"context"
	"os"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"go.datum.net/o11y/queryapi/internal/logql"
	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// logsDDL mirrors config/clickhouse-migrations/migrations/000001_init.up.sql
// so an integration test can build the exact production schema without
// reaching across module boundaries for the migration file.
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
    ProjectId String MATERIALIZED ResourceAttributes['milo.project.id']
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
	// projectContext); grant the connecting user custom-setting rights, as the
	// queryapi user is granted in production and in the migrate test.
	if err := boot.Exec(context.Background(),
		"GRANT settings_allow_custom_setting_read, settings_allow_custom_setting_write ON *.* TO "+opts.Auth.Username); err != nil {
		t.Fatalf("grant custom settings: %v", err)
	}

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
	runSeries(t, store, ctx, ran)

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
			map[string]string{"milo.project.id": "proj-a", "resource_name": "gateway-us-east"},
			map[string]string{"http_method": "GET"},
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
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
	for _, want := range []string{"service_name", "severity", "resource_name", "http_method", "milo.project.id"} {
		if !containsStr(names, want) {
			t.Errorf("LabelNames missing %q, got %v", want, names)
		}
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
