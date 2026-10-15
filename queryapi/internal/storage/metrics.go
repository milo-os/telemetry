// SPDX-License-Identifier: AGPL-3.0-only

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var (
	// ErrBackendUnavailable is a metrics backend that could not be reached or
	// answered with something other than a Prometheus API response.
	ErrBackendUnavailable = errors.New("storage: metrics backend unavailable")

	// ErrResponseTooLarge is a backend response over the configured size limit.
	ErrResponseTooLarge = errors.New("storage: backend response exceeds the size limit")
)

// QueryError is a query the backend rejected. Message is the backend's own
// text and is safe to return to the caller.
type QueryError struct{ Message string }

func (e *QueryError) Error() string { return "storage: query rejected: " + e.Message }

// InstantQuery is a PromQL query evaluated at one instant.
type InstantQuery struct {
	Query   string
	Time    time.Time
	Timeout time.Duration // 0: the backend's default
}

// RangeQuery is a PromQL query evaluated at each Step across Range.
type RangeQuery struct {
	Query   string
	Range   TimeRange
	Step    time.Duration
	Timeout time.Duration
}

// MetricStore is a backend for the Prometheus query API.
//
// Each method returns the "data" member of the backend's success envelope
// unchanged; handlers add the envelope. Implementations confine every call to
// the project on ctx and return ErrNoProject without contacting the backend
// when there is none. A zero Start or End in a TimeRange is left to the
// backend's default.
type MetricStore interface {
	Query(ctx context.Context, q InstantQuery) (json.RawMessage, error)
	QueryRange(ctx context.Context, q RangeQuery) (json.RawMessage, error)
	Series(ctx context.Context, matchers []string, tr TimeRange) (json.RawMessage, error)
	LabelNames(ctx context.Context, matchers []string, tr TimeRange) (json.RawMessage, error)
	LabelValues(ctx context.Context, name string, matchers []string, tr TimeRange) (json.RawMessage, error)
	Ping(ctx context.Context) error
}

var labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidLabelName reports whether name is a Prometheus label name.
func ValidLabelName(name string) bool { return labelNameRE.MatchString(name) }
