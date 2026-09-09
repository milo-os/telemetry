// SPDX-License-Identifier: AGPL-3.0-only

// Package clickhouse is the ClickHouse-backed storage.LogStore. It connects
// and serves tenant-scoped log queries over the o11y.logs schema.
package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"sort"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"go.datum.net/o11y/queryapi/internal/logql"
	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// Store queries logs from ClickHouse.
type Store struct {
	conn driver.Conn
}

// New opens a lazy connection; nothing is dialled until Ping or a query. TLS
// is mandatory: an unloadable certificate is an error, never a silent
// downgrade to plaintext.
func New(cfg Config) (*Store, error) {
	tlsCfg, err := cfg.tlsConfig()
	if err != nil {
		return nil, err
	}

	conn, err := ch.Open(&ch.Options{
		Addr: []string{fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)},
		Auth: ch.Auth{Database: cfg.Database, Username: cfg.User},
		TLS:  tlsCfg,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open: %w", err)
	}
	return &Store{conn: conn}, nil
}

// newStoreWithConn exists for tests, which cannot call New without real
// certificate files on disk.
func newStoreWithConn(conn driver.Conn) *Store {
	return &Store{conn: conn}
}

// Close releases the connection pool.
func (s *Store) Close() error { return s.conn.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.conn.Ping(ctx) }

// project returns the request's project, enforcing tenancy at the backend so a
// handler bug cannot issue an unscoped query.
func project(ctx context.Context) (string, error) {
	id, ok := miloauth.ProjectID(ctx)
	if !ok {
		return "", storage.ErrNoProject
	}
	return id, nil
}

// projectContext re-binds ctx so the ClickHouse query carries the request's
// project as the custom setting telemetry_project_id. The 000001_init
// migration's row policy on logs is ProjectId = getSetting('telemetry_project_id')
// TO queryapi, so every query must set it or match no rows.
func projectContext(ctx context.Context, project string) context.Context {
	return ch.Context(ctx, ch.WithSettings(ch.Settings{
		"telemetry_project_id": ch.CustomSetting{Value: project},
	}))
}

func (s *Store) QueryLogs(ctx context.Context, q storage.LogQuery) (storage.LogIterator, error) {
	projectID, err := project(ctx)
	if err != nil {
		return nil, err
	}
	query, args, err := buildLogsQuery(projectID, q)
	if err != nil {
		return nil, err
	}
	rows, err := s.conn.Query(projectContext(ctx, projectID), query, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: query logs: %w", err)
	}
	return &rowIterator{rows: rows}, nil
}

func (s *Store) LabelNames(ctx context.Context, tr storage.TimeRange) ([]string, error) {
	projectID, err := project(ctx)
	if err != nil {
		return nil, err
	}
	query, args := buildLabelNamesQuery(projectID, tr)
	names, err := s.scanStrings(projectContext(ctx, projectID), query, args)
	if err != nil {
		return nil, err
	}
	// The fixed schema columns are always in the catalogue regardless of data
	// (openapi.yaml's /labels contract), so merge them and re-sort+dedupe.
	names = append(withoutInternalAttributes(names), fixedSchemaLabels()...)
	sort.Strings(names)
	return dedupe(names), nil
}

func (s *Store) LabelValues(ctx context.Context, label string, tr storage.TimeRange) ([]string, error) {
	projectID, err := project(ctx)
	if err != nil {
		return nil, err
	}
	query, args := buildLabelValuesQuery(projectID, label, tr)
	values, err := s.scanStrings(projectContext(ctx, projectID), query, args)
	if err != nil {
		return nil, err
	}
	return nonEmpty(values), nil
}

func (s *Store) Series(ctx context.Context, matchers []logql.LabelMatcher, tr storage.TimeRange) (out []storage.LabelSet, err error) {
	projectID, err := project(ctx)
	if err != nil {
		return nil, err
	}
	query, args := buildSeriesQuery(projectID, matchers, tr)
	rows, err := s.conn.Query(projectContext(ctx, projectID), query, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: series: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	for rows.Next() {
		values := make([]string, len(seriesAllowlist))
		dest := make([]any, len(seriesAllowlist))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("clickhouse: series: scan: %w", err)
		}
		ls := storage.LabelSet{}
		for i, slot := range seriesAllowlist {
			if values[i] != "" {
				ls[slot.label] = values[i]
			}
		}
		out = append(out, ls)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: series: %w", err)
	}
	return out, nil
}

// scanStrings drains a single-string-column result (label names/values) into a
// sorted, deduped slice.
func (s *Store) scanStrings(ctx context.Context, query string, args []any) (out []string, err error) {
	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	seen := map[string]bool{}
	var v string
	for rows.Next() {
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("clickhouse: scan: %w", err)
		}
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

func nonEmpty(values []string) []string {
	out := values[:0]
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// dedupe removes adjacent duplicates from a sorted slice.
func dedupe(values []string) []string {
	out := values[:0]
	for _, v := range values {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}
