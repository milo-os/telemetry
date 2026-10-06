// SPDX-License-Identifier: AGPL-3.0-only

package logql

import (
	"regexp"
	"testing"
)

func TestSelectorString(t *testing.T) {
	tests := []struct {
		name  string
		build func(*Selector)
		want  string
	}{
		{
			name:  "quotes and backslashes are escaped",
			build: func(s *Selector) { s.Eq("a", `say "hi" \o/`) },
			want:  `{a="say \"hi\" \\o/"}`,
		},
		{
			name:  "one value uses equality",
			build: func(s *Selector) { s.OneOf("a", "x.y").NoneOf("b", "x.y") },
			want:  `{a="x.y", b!="x.y"}`,
		},
		{
			name:  "several values use an escaped alternation",
			build: func(s *Selector) { s.OneOf("a", "p.q", "z+").NoneOf("b", "x", "y") },
			want:  `{a=~"p\\.q|z\\+", b!~"x|y"}`,
		},
		{
			name:  "blank and repeated values collapse",
			build: func(s *Selector) { s.OneOf("a", "x", " ", "x") },
			want:  `{a="x"}`,
		},
		{
			name:  "no values add nothing",
			build: func(s *Selector) { s.Eq("a", "1").OneOf("b").NoneOf("c").Contains("") },
			want:  `{a="1"}`,
		},
		{
			name:  "line filters follow the selector in order",
			build: func(s *Selector) { s.Eq("a", "1").Contains(`"quoted"`).Excludes("health") },
			want:  `{a="1"} |= "\"quoted\"" != "health"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s Selector
			tt.build(&s)
			if got := s.String(); got != tt.want {
				t.Errorf("String() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestClone(t *testing.T) {
	var base Selector
	base.Eq("a", "1")
	c := base.Clone()
	c.Eq("b", "2").Contains("x")
	if got := base.String(); got != `{a="1"}` {
		t.Errorf("base changed to %s after extending its clone", got)
	}
}

func TestAlternation(t *testing.T) {
	// The query API anchors matcher regexes, as Loki does.
	re := regexp.MustCompile("^(?:" + Alternation("api", "api.v2") + ")$")
	for s, want := range map[string]bool{"api": true, "api.v2": true, "apixv2": false, "api-other": false} {
		if got := re.MatchString(s); got != want {
			t.Errorf("matches %q = %v, want %v", s, got, want)
		}
	}
}
