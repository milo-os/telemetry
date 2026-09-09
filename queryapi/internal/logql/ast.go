// SPDX-License-Identifier: AGPL-3.0-only

// Package logql parses the LogQL subset the query layer accepts: label
// matchers and line filters.
package logql

import (
	"regexp"
	"strings"
)

// MatchOperator is a LogQL label-matcher operator.
type MatchOperator string

const (
	MatchEqual     MatchOperator = "="
	MatchNotEqual  MatchOperator = "!="
	MatchRegexp    MatchOperator = "=~"
	MatchNotRegexp MatchOperator = "!~"
)

// LineFilterOperator is a LogQL line-filter operator.
type LineFilterOperator string

const (
	LineContains       LineFilterOperator = "|="
	LineNotContains    LineFilterOperator = "!="
	LineMatchesRegexp  LineFilterOperator = "|~"
	LineNotMatchRegexp LineFilterOperator = "!~"
)

// LabelMatcher constrains a query to log lines whose resolved label value
// compares against Value under Operator. Regexp is set only for regex
// operators, anchored as Loki anchors label matchers.
type LabelMatcher struct {
	Label    string
	Operator MatchOperator
	Value    string
	Regexp   *regexp.Regexp
}

func (m LabelMatcher) Matches(value string) bool {
	switch m.Operator {
	case MatchEqual:
		return value == m.Value
	case MatchNotEqual:
		return value != m.Value
	case MatchRegexp:
		return m.Regexp.MatchString(value)
	case MatchNotRegexp:
		return !m.Regexp.MatchString(value)
	}
	return false
}

// LineFilter constrains a query to log lines whose Body matches Value under
// Operator. Regexp is unanchored, as Loki's line filters are.
type LineFilter struct {
	Operator LineFilterOperator
	Value    string
	Regexp   *regexp.Regexp
}

func (f LineFilter) Matches(line string) bool {
	switch f.Operator {
	case LineContains:
		return strings.Contains(line, f.Value)
	case LineNotContains:
		return !strings.Contains(line, f.Value)
	case LineMatchesRegexp:
		return f.Regexp.MatchString(line)
	case LineNotMatchRegexp:
		return !f.Regexp.MatchString(line)
	}
	return false
}

// Query is the parsed form of a LogQL query string.
type Query struct {
	Matchers []LabelMatcher
	Filters  []LineFilter
}
