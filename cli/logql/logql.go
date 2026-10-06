// SPDX-License-Identifier: AGPL-3.0-only

// Package logql builds queries in the LogQL subset the telemetry query API
// accepts: one stream selector of label matchers, then line filters.
//
// The query API rejects `{a} or {b}`, so every condition a caller wants has to
// fit in one selector. Several values for one label become an escaped regex
// alternation rather than several selectors.
package logql

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Label-matcher operators.
const (
	OpEqual     = "="
	OpNotEqual  = "!="
	OpRegexp    = "=~"
	OpNotRegexp = "!~"
)

// Line-filter operators.
const (
	LineContains    = "|="
	LineNotContains = "!="
	LineMatches     = "|~"
	LineNotMatches  = "!~"
)

// Matcher constrains one stream label.
type Matcher struct {
	Label string
	Op    string
	Value string
}

// Filter constrains the log line itself.
type Filter struct {
	Op    string
	Value string
}

// Selector is one stream selector with its line filters. The zero value is
// empty; the query API rejects a selector with no matchers.
type Selector struct {
	Matchers []Matcher
	Filters  []Filter
}

// Eq requires label to equal value.
func (s *Selector) Eq(label, value string) *Selector {
	return s.add(label, OpEqual, value)
}

// Neq requires label not to equal value. A stream without the label matches.
func (s *Selector) Neq(label, value string) *Selector {
	return s.add(label, OpNotEqual, value)
}

// Re requires label to match pattern. The query API anchors it, as Loki does.
func (s *Selector) Re(label, pattern string) *Selector {
	return s.add(label, OpRegexp, pattern)
}

// Nre requires label not to match pattern.
func (s *Selector) Nre(label, pattern string) *Selector {
	return s.add(label, OpNotRegexp, pattern)
}

// OneOf requires label to equal one of values. No values adds nothing.
func (s *Selector) OneOf(label string, values ...string) *Selector {
	switch values = dedupe(values); len(values) {
	case 0:
		return s
	case 1:
		return s.Eq(label, values[0])
	default:
		return s.Re(label, Alternation(values...))
	}
}

// NoneOf excludes values of label. A stream without the label still matches.
func (s *Selector) NoneOf(label string, values ...string) *Selector {
	switch values = dedupe(values); len(values) {
	case 0:
		return s
	case 1:
		return s.Neq(label, values[0])
	default:
		return s.Nre(label, Alternation(values...))
	}
}

// Contains keeps lines containing text, matched case-sensitively. Empty text
// adds nothing.
func (s *Selector) Contains(text string) *Selector {
	if text != "" {
		s.Filters = append(s.Filters, Filter{LineContains, text})
	}
	return s
}

// Excludes drops lines containing text. Empty text adds nothing.
func (s *Selector) Excludes(text string) *Selector {
	if text != "" {
		s.Filters = append(s.Filters, Filter{LineNotContains, text})
	}
	return s
}

// Clone returns a copy that can be extended without changing s.
func (s Selector) Clone() Selector {
	s.Matchers = slices.Clone(s.Matchers)
	s.Filters = slices.Clone(s.Filters)
	return s
}

// String renders the query. String literals use Go quoting, which the query
// API unquotes with strconv.Unquote.
func (s Selector) String() string {
	parts := make([]string, len(s.Matchers))
	for i, m := range s.Matchers {
		parts[i] = m.Label + m.Op + strconv.Quote(m.Value)
	}
	var b strings.Builder
	b.WriteString("{" + strings.Join(parts, ", ") + "}")
	for _, f := range s.Filters {
		b.WriteString(" " + f.Op + " " + strconv.Quote(f.Value))
	}
	return b.String()
}

func (s *Selector) add(label, op, value string) *Selector {
	s.Matchers = append(s.Matchers, Matcher{label, op, value})
	return s
}

// Alternation is a regex matching exactly one of values, each taken literally.
func Alternation(values ...string) string {
	escaped := make([]string, len(values))
	for i, v := range values {
		escaped[i] = regexp.QuoteMeta(v)
	}
	return strings.Join(escaped, "|")
}

// dedupe drops blank and repeated values, keeping first-seen order.
func dedupe(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
