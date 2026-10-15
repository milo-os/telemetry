// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"context"
	"encoding/json"
	"time"

	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// FakeMetric is the one series the fake metric store reports.
const FakeMetric = "fake_up"

const projectLabel = "resourcemanager_datumapis_com_project_name"

// Metrics is a synthetic storage.MetricStore. It does not evaluate PromQL:
// every query answers with FakeMetric for the caller's project, so handler
// and e2e tests get a well-formed Prometheus response.
type Metrics struct{}

// NewMetrics returns a fake metric store.
func NewMetrics() *Metrics { return &Metrics{} }

var _ storage.MetricStore = (*Metrics)(nil)

func (*Metrics) labels(ctx context.Context) (map[string]string, error) {
	project, ok := miloauth.ProjectID(ctx)
	if !ok {
		return nil, storage.ErrNoProject
	}
	return map[string]string{"__name__": FakeMetric, projectLabel: project}, nil
}

func sample(t time.Time) []any { return []any{float64(t.UnixMilli()) / 1000, "1"} }

func (m *Metrics) Query(ctx context.Context, q storage.InstantQuery) (json.RawMessage, error) {
	ls, err := m.labels(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"resultType": "vector",
		"result":     []any{map[string]any{"metric": ls, "value": sample(q.Time)}},
	})
}

func (m *Metrics) QueryRange(ctx context.Context, q storage.RangeQuery) (json.RawMessage, error) {
	ls, err := m.labels(ctx)
	if err != nil {
		return nil, err
	}
	values := [][]any{}
	for t := q.Range.Start; !t.After(q.Range.End); t = t.Add(q.Step) {
		values = append(values, sample(t))
	}
	return json.Marshal(map[string]any{
		"resultType": "matrix",
		"result":     []any{map[string]any{"metric": ls, "values": values}},
	})
}

func (m *Metrics) Series(ctx context.Context, _ []string, _ storage.TimeRange) (json.RawMessage, error) {
	ls, err := m.labels(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal([]map[string]string{ls})
}

func (m *Metrics) LabelNames(ctx context.Context, _ []string, _ storage.TimeRange) (json.RawMessage, error) {
	if _, err := m.labels(ctx); err != nil {
		return nil, err
	}
	return json.Marshal([]string{"__name__", projectLabel})
}

func (m *Metrics) LabelValues(ctx context.Context, name string, _ []string, _ storage.TimeRange) (json.RawMessage, error) {
	ls, err := m.labels(ctx)
	if err != nil {
		return nil, err
	}
	values := []string{}
	if v, ok := ls[name]; ok {
		values = append(values, v)
	}
	return json.Marshal(values)
}

func (*Metrics) Ping(context.Context) error { return nil }
