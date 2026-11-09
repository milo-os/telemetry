// SPDX-License-Identifier: AGPL-3.0-only

package logql

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIsMetric(t *testing.T) {
	for raw, want := range map[string]bool{
		`sum by (a) (count_over_time({a="b"}[5m]))`: true,
		` sum(count_over_time({a="b"}[5m]))`:        true,
		`count_over_time({a="b"}[5m])`:              true,
		`{a="b"}`:                                   false,
		`summary`:                                   false,
		`{sum="x"}`:                                 false,
	} {
		if got := IsMetric(raw); got != want {
			t.Errorf("IsMetric(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseMetricForms(t *testing.T) {
	tests := []struct {
		raw   string
		by    []string
		rng   time.Duration
		match int
		lines int
	}{
		{`sum by (response_code) (count_over_time({route_name="my-alb"}[1800s]))`, []string{"response_code"}, 30 * time.Minute, 1, 0},
		{`sum(count_over_time({route_name="a", method="GET"} |= "x" [1h])) by (host, method)`, []string{"host", "method"}, time.Hour, 2, 1},
		{`sum(count_over_time({a="b"}[5m]))`, nil, 5 * time.Minute, 1, 0},
		{`sum by(a,a)(count_over_time({a=~"x[0-9]+"} |~ "(GET|POST) [a]" [1d]))`, []string{"a"}, 24 * time.Hour, 1, 1},
		{"sum by (a) (count_over_time({a=\"b\"} |= `)` [5m]))", []string{"a"}, 5 * time.Minute, 1, 1},
		{"sum(count_over_time({a=\"b\"} |= `[` != \"(\" [5m]))", nil, 5 * time.Minute, 1, 2},
	}
	for _, tt := range tests {
		q, err := ParseMetric(tt.raw)
		if err != nil {
			t.Fatalf("ParseMetric(%q): %v", tt.raw, err)
		}
		if !reflect.DeepEqual(q.By, tt.by) {
			t.Errorf("%q: By = %v, want %v", tt.raw, q.By, tt.by)
		}
		if q.Range != tt.rng {
			t.Errorf("%q: Range = %v, want %v", tt.raw, q.Range, tt.rng)
		}
		if len(q.Log.Matchers) != tt.match || len(q.Log.Filters) != tt.lines {
			t.Errorf("%q: %d matchers, %d filters; want %d, %d", tt.raw, len(q.Log.Matchers), len(q.Log.Filters), tt.match, tt.lines)
		}
	}
}

func TestParseMetricRejects(t *testing.T) {
	for raw, want := range map[string]string{
		`count_over_time({a="b"}[5m])`:                          "wrap count_over_time in sum",
		`sum(rate({a="b"}[5m]))`:                                "only sum(count_over_time",
		`sum(count_over_time({a="b"}))`:                         "needs a range",
		`sum(count_over_time({a="b"}[0s]))`:                     "invalid range",
		`sum(count_over_time({a="b"}[soon]))`:                   "invalid range",
		`sum by () (count_over_time({a="b"}[5m]))`:              "at least one label",
		`sum by (a.b) (count_over_time({a="b"}[5m]))`:           "invalid label name",
		`sum by (a, b, c, d, e) (count_over_time({a="b"}[5m]))`: "at most 4",
		`sum by (a) (count_over_time({a="b"}[5m])) by (b)`:      "by given twice",
		`sum(count_over_time({a="b"}[5m])) + 1`:                 "unexpected",
		`sum(count_over_time({a="b"} | json [5m]))`:             `"json" is not supported`,
		`sum(count_over_time({a="b"}[5m])`:                      "unbalanced",
		"sum(count_over_time({a=\"b\"} |= `)[5m]))":             "unterminated string",
	} {
		_, err := ParseMetric(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseMetric(%q) error = %v, want it to mention %q", raw, err, want)
		}
	}
}
