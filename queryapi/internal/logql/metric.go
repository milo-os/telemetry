// SPDX-License-Identifier: AGPL-3.0-only

package logql

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// MaxGroupingLabels bounds `sum by (...)`. Each label multiplies the number of
// groups a count can return, so the subset stays small enough to answer from
// one GROUP BY.
const MaxGroupingLabels = 4

// MetricQuery is the one metric form the query layer accepts: a sum of
// count_over_time over a log query, optionally grouped by labels.
//
//	sum by (response_code) (count_over_time({route_name="a"} |= "x" [30m]))
//	sum(count_over_time({route_name="a"}[30m])) by (response_code)
//	sum(count_over_time({route_name="a"}[30m]))
//
// It answers "how many lines per value of these labels in this window", which
// is what a facet count needs, without a general metric engine.
type MetricQuery struct {
	Log   Query
	Range time.Duration
	// By is empty for an ungrouped total.
	By []string
}

// IsMetric reports whether raw is shaped like a metric query rather than a
// stream selector, so a handler can pick the parser without parsing twice.
func IsMetric(raw string) bool {
	s := strings.TrimSpace(raw)
	return hasWord(s, "sum") || hasWord(s, "count_over_time")
}

// hasWord reports whether s starts with word followed by a space or "(".
func hasWord(s, word string) bool {
	if !strings.HasPrefix(s, word) || len(s) == len(word) {
		return false
	}
	next := s[len(word)]
	return next == '(' || next == ' ' || next == '\t' || next == '\n'
}

// ParseMetric parses the supported metric form. Anything else, including
// rate, bytes_over_time, other aggregations or a bare count_over_time, is
// rejected with a message naming the accepted shape.
func ParseMetric(raw string) (*MetricQuery, error) {
	s := strings.TrimSpace(raw)
	if hasWord(s, "count_over_time") {
		return nil, errors.New("logql: wrap count_over_time in sum, e.g. " +
			"sum by (severity) (count_over_time({service_name=\"x\"}[5m]))")
	}
	if !hasWord(s, "sum") {
		return nil, errors.New("logql: only sum(count_over_time(...)) metric queries are supported")
	}
	s = strings.TrimSpace(s[len("sum"):])

	var by []string
	var err error
	if hasWord(s, "by") {
		if by, s, err = parseGrouping(strings.TrimSpace(s[len("by"):])); err != nil {
			return nil, err
		}
	}

	if !strings.HasPrefix(s, "(") {
		return nil, fmt.Errorf("logql: expected ( after sum, got %q", s)
	}
	end, err := closingParen(s)
	if err != nil {
		return nil, err
	}
	inner := strings.TrimSpace(s[1:end])
	rest := strings.TrimSpace(s[end+1:])

	if hasWord(rest, "by") {
		if by != nil {
			return nil, errors.New("logql: by given twice")
		}
		if by, rest, err = parseGrouping(strings.TrimSpace(rest[len("by"):])); err != nil {
			return nil, err
		}
	}
	if rest != "" {
		return nil, fmt.Errorf("logql: unexpected %q after sum(...)", rest)
	}

	log, rng, err := parseCountOverTime(inner)
	if err != nil {
		return nil, err
	}
	return &MetricQuery{Log: *log, Range: rng, By: by}, nil
}

// parseGrouping reads "(a, b)" and returns the labels and what follows.
func parseGrouping(s string) ([]string, string, error) {
	if !strings.HasPrefix(s, "(") {
		return nil, "", fmt.Errorf("logql: expected ( after by, got %q", s)
	}
	end := strings.IndexByte(s, ')')
	if end < 0 {
		return nil, "", errors.New("logql: unclosed by (...)")
	}
	labels := []string{}
	seen := map[string]bool{}
	for _, part := range strings.Split(s[1:end], ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		ident, tail, err := scanIdent(name)
		if err != nil || tail != "" {
			return nil, "", fmt.Errorf("logql: invalid label name %q in by (...)", name)
		}
		if !seen[ident] {
			seen[ident] = true
			labels = append(labels, ident)
		}
	}
	if len(labels) == 0 {
		return nil, "", errors.New("logql: by (...) needs at least one label")
	}
	if len(labels) > MaxGroupingLabels {
		return nil, "", fmt.Errorf("logql: by (...) accepts at most %d labels", MaxGroupingLabels)
	}
	return labels, strings.TrimSpace(s[end+1:]), nil
}

// parseCountOverTime reads `count_over_time(<log query> [<range>])`.
func parseCountOverTime(s string) (*Query, time.Duration, error) {
	if !hasWord(s, "count_over_time") {
		return nil, 0, errors.New("logql: only sum(count_over_time(...)) metric queries are supported")
	}
	s = strings.TrimSpace(s[len("count_over_time"):])
	if !strings.HasPrefix(s, "(") {
		return nil, 0, fmt.Errorf("logql: expected ( after count_over_time, got %q", s)
	}
	end, err := closingParen(s)
	if err != nil {
		return nil, 0, err
	}
	if tail := strings.TrimSpace(s[end+1:]); tail != "" {
		return nil, 0, fmt.Errorf("logql: unexpected %q after count_over_time(...)", tail)
	}
	body := strings.TrimSpace(s[1:end])

	open := lastUnquoted(body, '[')
	if open < 0 || !strings.HasSuffix(body, "]") {
		return nil, 0, errors.New("logql: count_over_time needs a range, e.g. [5m]")
	}
	rng, err := model.ParseDuration(strings.TrimSpace(body[open+1 : len(body)-1]))
	if err != nil || rng <= 0 {
		return nil, 0, fmt.Errorf("logql: invalid range %q", body[open:])
	}

	log, err := Parse(body[:open])
	if err != nil {
		return nil, 0, err
	}
	return log, time.Duration(rng), nil
}

// closingParen returns the index of the ")" matching s[0] == "(", skipping
// quoted strings so a regex like "(a|b)" in a filter cannot unbalance it.
func closingParen(s string) (int, error) {
	depth := 0
	for i := 0; i < len(s); i++ {
		if isQuote(s[i]) {
			n, err := quotedLen(s[i:])
			if err != nil {
				return 0, err
			}
			i += n - 1
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, errors.New("logql: unbalanced parentheses")
}

// lastUnquoted returns the index of the last c outside a quoted string, or -1.
// An unterminated string ends the scan; Parse reports it.
func lastUnquoted(s string, c byte) int {
	last := -1
	for i := 0; i < len(s); i++ {
		if isQuote(s[i]) {
			n, err := quotedLen(s[i:])
			if err != nil {
				return last
			}
			i += n - 1
			continue
		}
		if s[i] == c {
			last = i
		}
	}
	return last
}

// isQuote reports whether b opens a string Parse accepts: "...", `...` or '...'.
func isQuote(b byte) bool {
	return b == '"' || b == '`' || b == '\''
}

// quotedLen returns the length of the string literal at the start of s, using
// the same rules as Parse, so backtick raw strings have no escapes.
func quotedLen(s string) (int, error) {
	quoted, err := strconv.QuotedPrefix(s)
	if err != nil {
		return 0, errors.New("logql: unterminated string")
	}
	return len(quoted), nil
}
