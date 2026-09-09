// SPDX-License-Identifier: AGPL-3.0-only

package fake_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.datum.net/o11y/queryapi/internal/logql"
	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
	"go.datum.net/o11y/queryapi/internal/storage/fake"
)

func testCtx() context.Context {
	return miloauth.WithProject(context.Background(), "proj-abc")
}

func testRange() storage.TimeRange {
	end := time.Unix(1767225600, 0).UTC() // 2026-01-01T00:00:00Z
	return storage.TimeRange{Start: end.Add(-15 * time.Minute), End: end}
}

// drain runs q against s and collects every row.
func drain(t *testing.T, s *fake.Store, q storage.LogQuery) []storage.Row {
	t.Helper()
	iter, err := s.QueryLogs(testCtx(), q)
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	var rows []storage.Row
	for iter.Next() {
		rows = append(rows, iter.Row())
	}
	if err := errors.Join(iter.Err(), iter.Close()); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return rows
}

func query(matchers []logql.LabelMatcher, filters []logql.LineFilter, limit int, dir storage.Direction) storage.LogQuery {
	return storage.LogQuery{
		Matchers: matchers, Filters: filters,
		Range: testRange(), Limit: limit, Direction: dir,
	}
}

func mustParse(t *testing.T, raw string) *logql.Query {
	t.Helper()
	q, err := logql.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	return q
}

func TestRequiresProject(t *testing.T) {
	s := fake.New(2)
	_, err := s.QueryLogs(context.Background(), query(nil, nil, 10, storage.DirectionBackward))
	if !errors.Is(err, storage.ErrNoProject) {
		t.Fatalf("QueryLogs without a project = %v, want ErrNoProject", err)
	}
	if _, err := s.LabelNames(context.Background(), testRange()); !errors.Is(err, storage.ErrNoProject) {
		t.Fatalf("LabelNames without a project = %v, want ErrNoProject", err)
	}
}

func TestDeterministic(t *testing.T) {
	s := fake.New(2)
	q := query(nil, nil, 50, storage.DirectionBackward)

	first := drain(t, s, q)
	second := drain(t, fake.New(2), q)

	if len(first) != len(second) {
		t.Fatalf("row counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if !first[i].Timestamp.Equal(second[i].Timestamp) || first[i].Line != second[i].Line {
			t.Fatalf("row %d differs:\n %+v\n %+v", i, first[i], second[i])
		}
	}
}

func TestBackwardReturnsNewestFirst(t *testing.T) {
	s := fake.New(2)
	rows := drain(t, s, query(nil, nil, 10, storage.DirectionBackward))

	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Timestamp.After(rows[i-1].Timestamp) {
			t.Fatalf("row %d is newer than row %d; backward must descend", i, i-1)
		}
	}

	// The newest row must sit near End, not near Start: a limit must not
	// truncate the oldest end of the window.
	tr := testRange()
	if tr.End.Sub(rows[0].Timestamp) > time.Minute {
		t.Errorf("newest row is %v before End; expected it adjacent to End", tr.End.Sub(rows[0].Timestamp))
	}
}

func TestForwardReturnsOldestFirst(t *testing.T) {
	s := fake.New(2)
	rows := drain(t, s, query(nil, nil, 10, storage.DirectionForward))

	for i := 1; i < len(rows); i++ {
		if rows[i].Timestamp.Before(rows[i-1].Timestamp) {
			t.Fatalf("row %d is older than row %d; forward must ascend", i, i-1)
		}
	}
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10", len(rows))
	}
	tr := testRange()
	if rows[0].Timestamp.Sub(tr.Start) > time.Minute {
		t.Errorf("oldest row is %v after Start", rows[0].Timestamp.Sub(tr.Start))
	}
}

func TestLabelMatcherFilters(t *testing.T) {
	s := fake.New(20)
	q := mustParse(t, `{service_name="waf"}`)
	rows := drain(t, s, query(q.Matchers, nil, 100, storage.DirectionBackward))

	if len(rows) == 0 {
		t.Fatal("no rows matched service_name=waf")
	}
	for _, row := range rows {
		if row.Labels["service_name"] != "waf" {
			t.Fatalf("row has service_name %q, want waf", row.Labels["service_name"])
		}
	}
}

func TestAttributeLabelMatcherFilters(t *testing.T) {
	s := fake.New(20)
	q := mustParse(t, `{resource_name="gateway-us-east"}`)
	rows := drain(t, s, query(q.Matchers, nil, 100, storage.DirectionBackward))

	if len(rows) == 0 {
		t.Fatal("no rows matched resource_name=gateway-us-east")
	}
	for _, row := range rows {
		if row.Labels["resource_name"] != "gateway-us-east" {
			t.Fatalf("row has resource_name %q", row.Labels["resource_name"])
		}
	}
}

func TestLineFilterAppliesBeforeLimit(t *testing.T) {
	s := fake.New(20)
	q := mustParse(t, `{service_name="envoy-gateway"} |= "GET"`)
	rows := drain(t, s, query(q.Matchers, q.Filters, 20, storage.DirectionBackward))

	if len(rows) != 20 {
		t.Fatalf("got %d rows, want the filter to keep scanning until 20 matched", len(rows))
	}
	for _, row := range rows {
		if !strings.Contains(row.Line, "GET") {
			t.Fatalf("row line %q does not contain GET", row.Line)
		}
	}
}

func TestNoMatchesIsEmptyNotError(t *testing.T) {
	s := fake.New(2)
	q := mustParse(t, `{service_name="does-not-exist"}`)
	rows := drain(t, s, query(q.Matchers, nil, 100, storage.DirectionBackward))
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

func TestDiscovery(t *testing.T) {
	s := fake.New(2)

	names, err := s.LabelNames(testCtx(), testRange())
	if err != nil {
		t.Fatalf("LabelNames: %v", err)
	}
	want := []string{"resource_name", "service_name", "severity"}
	if len(names) != len(want) {
		t.Fatalf("LabelNames = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("LabelNames = %v, want %v (sorted)", names, want)
		}
	}

	values, err := s.LabelValues(testCtx(), "severity", testRange())
	if err != nil {
		t.Fatalf("LabelValues: %v", err)
	}
	if len(values) != 4 {
		t.Fatalf("severity values = %v, want 4", values)
	}

	if _, err := s.LabelValues(testCtx(), "nope", testRange()); err != nil {
		t.Fatalf("LabelValues for an unknown label should be empty, not an error: %v", err)
	}

	series, err := s.Series(testCtx(), mustParse(t, `{service_name="waf"}`).Matchers, testRange())
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(series) == 0 {
		t.Fatal("Series returned nothing for service_name=waf")
	}
	for _, ls := range series {
		if ls["service_name"] != "waf" {
			t.Fatalf("series %v does not match the matcher", ls)
		}
	}
}

func TestRejectsNonPositiveLimit(t *testing.T) {
	s := fake.New(2)
	for _, limit := range []int{0, -1} {
		if _, err := s.QueryLogs(testCtx(), query(nil, nil, limit, storage.DirectionBackward)); !errors.Is(err, storage.ErrInvalidLimit) {
			t.Errorf("QueryLogs with limit %d = %v, want ErrInvalidLimit", limit, err)
		}
	}
}

func TestPing(t *testing.T) {
	if err := fake.New(2).Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
