// SPDX-License-Identifier: AGPL-3.0-only

// Package storage defines the log query interface the HTTP handlers depend on.
// It is shaped for ClickHouse -- pushdown fields mirror SQL clauses and rows
// stream through an iterator -- and other backends adapt to that.
package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.datum.net/o11y/queryapi/internal/logql"
)

var (
	// ErrNotImplemented is returned by backends that cannot yet serve a method.
	ErrNotImplemented = errors.New("storage: not implemented")

	// ErrNoProject is returned when no project is present on the context.
	// Backends check this themselves so a handler bug cannot issue an
	// unscoped query.
	ErrNoProject = errors.New("storage: no project on request context")

	// ErrInvalidLimit is returned for a non-positive Limit. Backends reject it
	// rather than treating it as unbounded.
	ErrInvalidLimit = errors.New("storage: limit must be greater than zero")
)

// TimeRange is a half-open interval [Start, End).
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// Direction is a result ordering, pushed down to ORDER BY.
type Direction string

const (
	DirectionBackward Direction = "backward" // newest first; Loki's default
	DirectionForward  Direction = "forward"
)

// LogQuery is a log query with all constraints pushed down. Callers must not
// re-apply Limit or Direction to the results.
type LogQuery struct {
	Matchers []logql.LabelMatcher
	Filters  []logql.LineFilter
	Range    TimeRange

	// Limit must be > 0; backends return ErrInvalidLimit for zero rather than
	// scanning an unbounded window.
	Limit     int
	Direction Direction
}

// LabelSet is a stream's labels.
type LabelSet map[string]string

// Key returns a stable identity for grouping rows into streams. Lengths are
// encoded so no byte sequence in a name or value can imitate a separator --
// attribute values are arbitrary log data.
func (l LabelSet) Key() string {
	pairs := make([]string, 0, len(l))
	for k, v := range l {
		pairs = append(pairs, fmt.Sprintf("%d:%s=%d:%s", len(k), k, len(v), v))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// Row is one log line with its resolved stream labels.
type Row struct {
	Timestamp time.Time
	Labels    LabelSet
	Line      string
}

// LogIterator streams rows, mirroring database/sql's Next/Err/Close so a
// ClickHouse driver result wraps without intermediate buffering. Callers
// needing Loki's grouped envelope buffer; LogQuery.Limit bounds that.
type LogIterator interface {
	Next() bool
	Row() Row
	Err() error
	Close() error
}

// LogStore is a log query backend.
type LogStore interface {
	QueryLogs(ctx context.Context, q LogQuery) (LogIterator, error)
	LabelNames(ctx context.Context, tr TimeRange) ([]string, error)
	LabelValues(ctx context.Context, label string, tr TimeRange) ([]string, error)

	// Series is called once per match[] selector; the handler unions and dedupes
	// the results.
	Series(ctx context.Context, matchers []logql.LabelMatcher, tr TimeRange) ([]LabelSet, error)

	Ping(ctx context.Context) error
}
