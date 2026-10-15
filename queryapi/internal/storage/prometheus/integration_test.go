// SPDX-License-Identifier: AGPL-3.0-only

package prometheus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
	"go.datum.net/o11y/queryapi/internal/storage/prometheus"
)

const itMetric = "queryapi_it_requests_total"

// seed imports a counter for proj-a (+1 per 15s, label only_a) and proj-b
// (+100 per 15s, label only_b) over the last ten minutes, then waits until VM
// serves both.
func seed(t *testing.T, base string, now time.Time) {
	t.Helper()
	var b strings.Builder
	for i := 0; i <= 40; i++ {
		ts := now.Add(-10 * time.Minute).Add(time.Duration(i) * 15 * time.Second).UnixMilli()
		fmt.Fprintf(&b, "%s{%s=\"proj-a\",only_a=\"x\"} %d %d\n", itMetric, label, i, ts)
		fmt.Fprintf(&b, "%s{%s=\"proj-b\",only_b=\"y\"} %d %d\n", itMetric, label, i*100, ts)
	}
	resp, err := http.Post(base+"/api/v1/import/prometheus", "text/plain", strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := resp.Body.Close(); err != nil || resp.StatusCode/100 != 2 {
		t.Fatalf("import: status %d, close %v", resp.StatusCode, err)
	}
	for range 60 {
		if resp, err := http.Get(base + "/internal/force_flush"); err == nil {
			_ = resp.Body.Close()
		}
		resp, err := http.Get(base + "/api/v1/series?match[]=" + itMetric)
		if err == nil {
			var r struct{ Data []map[string]string }
			decodeErr := json.NewDecoder(resp.Body).Decode(&r)
			_ = resp.Body.Close()
			if decodeErr == nil && len(r.Data) == 2 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("seeded series never became visible")
}

type result struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

func series(t *testing.T, raw json.RawMessage) []map[string]string {
	t.Helper()
	var r result
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var out []struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	if err := json.Unmarshal(r.Result, &out); err != nil {
		t.Fatalf("decode result %s: %v", r.Result, err)
	}
	var ls []map[string]string
	for _, s := range out {
		ls = append(ls, s.Metric)
	}
	return ls
}

func projects(ls []map[string]string) []string {
	var ps []string
	for _, l := range ls {
		ps = append(ps, l[label])
	}
	slices.Sort(ps)
	return slices.Compact(ps)
}

func TestStoreAgainstVictoriaMetrics(t *testing.T) {
	base := os.Getenv("PROMETHEUS_TEST_URL")
	if base == "" {
		t.Skip("PROMETHEUS_TEST_URL not set; run `task test-integration-prometheus`")
	}
	now := time.Now().UTC().Truncate(time.Second)
	seed(t, base, now)

	s, err := prometheus.New(prometheus.Config{
		URL: base, Tenancy: prometheus.TenancyExtraLabel, ProjectLabel: label, MaxResponseBytes: 32 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	ctx := miloauth.WithProject(context.Background(), "proj-a")
	tr := storage.TimeRange{Start: now.Add(-15 * time.Minute), End: now}

	query := func(q string) json.RawMessage {
		t.Helper()
		raw, err := s.Query(ctx, storage.InstantQuery{Query: q, Time: now})
		if err != nil {
			t.Fatalf("Query(%s): %v", q, err)
		}
		return raw
	}

	// Positive: proj-a sees its own series.
	if got := projects(series(t, query(itMetric))); !slices.Equal(got, []string{"proj-a"}) {
		t.Errorf("own series: projects = %v, want [proj-a]", got)
	}

	// Negative: naming proj-b, or selecting everything, yields nothing of
	// proj-b's.
	if got := series(t, query(itMetric+`{`+label+`="proj-b"}`)); len(got) != 0 {
		t.Errorf("explicit proj-b selector returned %v", got)
	}
	if got := projects(series(t, query(`{__name__=~"queryapi_it_.+"}`))); !slices.Equal(got, []string{"proj-a"}) {
		t.Errorf("match-all selector: projects = %v, want [proj-a]", got)
	}

	// An aggregate sees only proj-a's value (40), not the sum with proj-b's
	// (4040).
	var sum []struct {
		Value []any `json:"value"`
	}
	var r result
	if err := json.Unmarshal(query("sum("+itMetric+")"), &r); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r.Result, &sum); err != nil || len(sum) != 1 || sum[0].Value[1] != "40" {
		t.Errorf("sum = %+v (%v), want 40", sum, err)
	}

	// The portal's reset-guarded increase parses and evaluates.
	portal := fmt.Sprintf("sum(increase(%[1]s[5m]) * (resets(%[1]s[5m]) == bool 0))", itMetric)
	if err := json.Unmarshal(query(portal), &r); err != nil || r.ResultType != "vector" {
		t.Errorf("portal query: resultType %q, err %v", r.ResultType, err)
	}

	// Grafana's health probe evaluates. VictoriaMetrics
	// answers a scalar expression as a vector, not a scalar.
	if err := json.Unmarshal(query("1+1"), &r); err != nil || r.ResultType != "vector" {
		t.Errorf("1+1: resultType %q, err %v; want vector", r.ResultType, err)
	}

	// Range query: positive and negative.
	raw, err := s.QueryRange(ctx, storage.RangeQuery{Query: `{__name__=~"queryapi_it_.+"}`, Range: tr, Step: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	var mat struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &mat); err != nil || len(mat.Result) != 1 || mat.Result[0].Metric[label] != "proj-a" {
		t.Errorf("range: %s (%v), want exactly proj-a's series", raw, err)
	}

	// Cache carry-over: with VictoriaMetrics' rollup cache on, a range
	// result computed for proj-b must not be served to proj-a.
	rangeFor := func(project, q string) string {
		t.Helper()
		raw, err := s.QueryRange(miloauth.WithProject(context.Background(), project),
			storage.RangeQuery{Query: q, Range: tr, Step: 30 * time.Second})
		if err != nil {
			t.Fatalf("QueryRange(%s, %s): %v", project, q, err)
		}
		return string(raw)
	}
	for _, q := range []string{`{__name__=~"queryapi_it_.+"}`, "sum(" + itMetric + ")"} {
		b1 := rangeFor("proj-b", q)
		a1 := rangeFor("proj-a", q)
		b2 := rangeFor("proj-b", q)
		if !strings.Contains(b1, "proj-b") && !strings.Contains(b1, `"4000"`) {
			t.Errorf("%s: proj-b result has no proj-b data (vacuous check): %s", q, b1)
		}
		if strings.Contains(a1, "proj-b") || strings.Contains(a1, "only_b") || strings.Contains(a1, `"4000"`) {
			t.Errorf("%s: proj-a result carries proj-b data: %s", q, a1)
		}
		if strings.Contains(b2, "proj-a") || strings.Contains(b2, "only_a") {
			t.Errorf("%s: proj-b result carries proj-a data: %s", q, b2)
		}
	}

	// Series.
	raw, err = s.Series(ctx, []string{`{__name__=~".+"}`}, tr)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	var sl []map[string]string
	if err := json.Unmarshal(raw, &sl); err != nil {
		t.Fatal(err)
	}
	if got := projects(sl); !slices.Equal(got, []string{"proj-a"}) {
		t.Errorf("series: projects = %v, want [proj-a]", got)
	}

	// Label names: only_a visible, only_b not.
	raw, err = s.LabelNames(ctx, nil, tr)
	if err != nil {
		t.Fatalf("LabelNames: %v", err)
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "only_a") || slices.Contains(names, "only_b") {
		t.Errorf("label names = %v, want only_a and not only_b", names)
	}

	// Label values.
	values := func(name string) []string {
		t.Helper()
		raw, err := s.LabelValues(ctx, name, nil, tr)
		if err != nil {
			t.Fatalf("LabelValues(%s): %v", name, err)
		}
		var vs []string
		if err := json.Unmarshal(raw, &vs); err != nil {
			t.Fatal(err)
		}
		return vs
	}
	if got := values(label); !slices.Equal(got, []string{"proj-a"}) {
		t.Errorf("project label values = %v, want [proj-a]", got)
	}
	if got := values("only_b"); len(got) != 0 {
		t.Errorf("only_b values = %v, want none", got)
	}
	if got := values("only_a"); !slices.Equal(got, []string{"x"}) {
		t.Errorf("only_a values = %v, want [x]", got)
	}
}
