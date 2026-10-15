// SPDX-License-Identifier: AGPL-3.0-only

package fake_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
	"go.datum.net/o11y/queryapi/internal/storage/fake"
)

func TestMetricsRequireAProject(t *testing.T) {
	m := fake.NewMetrics()
	ctx := context.Background()
	tr := storage.TimeRange{Start: time.Unix(0, 0), End: time.Unix(60, 0)}
	calls := map[string]func() error{
		"query": func() error { _, err := m.Query(ctx, storage.InstantQuery{Query: "up"}); return err },
		"query_range": func() error {
			_, err := m.QueryRange(ctx, storage.RangeQuery{Query: "up", Range: tr, Step: time.Second})
			return err
		},
		"series":       func() error { _, err := m.Series(ctx, []string{"up"}, tr); return err },
		"labels":       func() error { _, err := m.LabelNames(ctx, nil, tr); return err },
		"label values": func() error { _, err := m.LabelValues(ctx, "__name__", nil, tr); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, storage.ErrNoProject) {
			t.Errorf("%s: err = %v, want ErrNoProject", name, err)
		}
	}
}

func TestMetricsAreScopedToTheCallersProject(t *testing.T) {
	m := fake.NewMetrics()
	ctx := miloauth.WithProject(context.Background(), "proj-a")

	raw, err := m.Query(ctx, storage.InstantQuery{Query: "up", Time: time.Unix(1760400000, 0)})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var vec struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if vec.ResultType != "vector" || len(vec.Result) != 1 {
		t.Fatalf("got %s with %d series, want one vector series", vec.ResultType, len(vec.Result))
	}
	if got := vec.Result[0].Metric["resourcemanager_datumapis_com_project_name"]; got != "proj-a" {
		t.Errorf("project label = %q, want proj-a", got)
	}

	values, err := m.LabelValues(ctx, "resourcemanager_datumapis_com_project_name", nil, storage.TimeRange{})
	if err != nil {
		t.Fatalf("LabelValues: %v", err)
	}
	var got []string
	if err := json.Unmarshal(values, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Equal(got, []string{"proj-a"}) {
		t.Errorf("project label values = %v, want [proj-a]", got)
	}
}

func TestMetricsRangeHasOnePointPerStep(t *testing.T) {
	m := fake.NewMetrics()
	ctx := miloauth.WithProject(context.Background(), "proj-a")
	raw, err := m.QueryRange(ctx, storage.RangeQuery{
		Query: "up",
		Range: storage.TimeRange{Start: time.Unix(0, 0), End: time.Unix(600, 0)},
		Step:  time.Minute,
	})
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	var mat struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Values [][]any `json:"values"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &mat); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mat.ResultType != "matrix" || len(mat.Result) != 1 || len(mat.Result[0].Values) != 11 {
		t.Fatalf("got %+v, want one matrix series of 11 points", mat)
	}
}
