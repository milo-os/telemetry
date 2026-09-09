// SPDX-License-Identifier: AGPL-3.0-only

package logql_test

import (
	"strings"
	"testing"

	"go.datum.net/o11y/queryapi/internal/logql"
)

func TestParseAccepts(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		matchers int
		filters  int
	}{
		{"single matcher", `{service_name="envoy-gateway"}`, 1, 0},
		{"two matchers", `{service_name="envoy-gateway", severity!="DEBUG"}`, 2, 0},
		{"regex matchers", `{service_name=~"envoy.*", severity!~"DEBUG|INFO"}`, 2, 0},
		{"one line filter", `{service_name="waf"} |= "blocked"`, 1, 1},
		{"chained line filters", `{service_name="waf"} |= "blocked" != "healthz" |~ "rule-[0-9]+" !~ "debug"`, 1, 4},
		{"value with comma", `{resource_name="a,b"}`, 1, 0},
		{"escaped quote in value", `{resource_name="say \"hi\""}`, 1, 0},
		{"extra whitespace", `  {  service_name = "waf" }   |=  "x"  `, 1, 1},
		{"regex quantifier braces", `{service_name=~"gw-[0-9]{2}"}`, 1, 0},
		{"literal brace in value", `{resource_name="a}b"}`, 1, 0},
		{"brace in value with filter", `{resource_name="a}b"} |= "x"`, 1, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := logql.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse(%q) = error %v, want success", tc.raw, err)
			}
			if len(q.Matchers) != tc.matchers {
				t.Errorf("got %d matchers, want %d", len(q.Matchers), tc.matchers)
			}
			if len(q.Filters) != tc.filters {
				t.Errorf("got %d filters, want %d", len(q.Filters), tc.filters)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", ``, "empty query"},
		{"empty selector", `{}`, "at least one label matcher"},
		{"aggregation", `rate({service_name="waf"}[5m])`, "not supported"},
		{"sum by", `sum by (service_name) (count_over_time({service_name="waf"}[5m]))`, "not supported"},
		{"range vector", `{service_name="waf"}[5m]`, "not supported"},
		{"json parser", `{service_name="waf"} | json`, "not supported"},
		{"logfmt parser", `{service_name="waf"} | logfmt`, "not supported"},
		{"line_format", `{service_name="waf"} | line_format "{{.x}}"`, "not supported"},
		{"label_format", `{service_name="waf"} | label_format x=y`, "not supported"},
		{"unclosed selector", `{service_name="waf"`, "unclosed"},
		{"unterminated string", `{service_name="waf}`, "unterminated"},
		{"missing operator", `{service_name}`, "expected"},
		{"bad label regexp", `{service_name=~"("}`, "invalid regexp"},
		{"bad filter regexp", `{service_name="waf"} |~ "("`, "invalid regexp"},
		{"trailing junk", `{service_name="waf"} wat`, "unexpected"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := logql.Parse(tc.raw)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want error", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse(%q) error = %q, want it to contain %q", tc.raw, err, tc.want)
			}
		})
	}
}

func TestLabelMatcherMatches(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		value string
		want  bool
	}{
		{"equal hit", `{severity="ERROR"}`, "ERROR", true},
		{"equal miss", `{severity="ERROR"}`, "INFO", false},
		{"not equal hit", `{severity!="ERROR"}`, "INFO", true},
		{"regex anchored hit", `{service_name=~"envoy.*"}`, "envoy-gateway", true},
		{"regex anchored miss", `{service_name=~"gateway"}`, "envoy-gateway", false},
		{"not regex hit", `{service_name!~"waf"}`, "envoy-gateway", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := logql.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.raw, err)
			}
			if got := q.Matchers[0].Matches(tc.value); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestLineFilterMatches(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		line string
		want bool
	}{
		{"contains hit", `{severity="ERROR"} |= "timeout"`, "upstream timeout", true},
		{"contains miss", `{severity="ERROR"} |= "timeout"`, "ok", false},
		{"not contains hit", `{severity="ERROR"} != "healthz"`, "GET /api", true},
		{"regex unanchored hit", `{severity="ERROR"} |~ "rule-[0-9]+"`, "matched rule-42 blocked", true},
		{"not regex hit", `{severity="ERROR"} !~ "rule-[0-9]+"`, "no rules here", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := logql.Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.raw, err)
			}
			if got := q.Filters[0].Matches(tc.line); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}
