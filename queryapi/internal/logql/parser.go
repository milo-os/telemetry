// SPDX-License-Identifier: AGPL-3.0-only

package logql

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// unsupported names LogQL constructs outside the accepted subset. Each is
// rejected explicitly rather than ignored.
var unsupported = []string{
	"json", "logfmt", "pattern", "regexp", "unpack",
	"line_format", "label_format", "unwrap", "drop", "keep", "decolorize",
}

// Parse parses a LogQL query into the supported subset: label matchers and
// line filters.
func Parse(raw string) (*Query, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("logql: empty query")
	}
	if s[0] != '{' {
		return nil, errors.New("logql: metric queries and aggregations are not supported; " +
			"a query must be a stream selector, e.g. {service_name=\"envoy-gateway\"}")
	}

	end, err := closingBrace(s)
	if err != nil {
		return nil, err
	}

	matchers, err := parseMatchers(s[1:end])
	if err != nil {
		return nil, err
	}
	if len(matchers) == 0 {
		return nil, errors.New("logql: at least one label matcher is required")
	}

	filters, err := parseFilters(strings.TrimSpace(s[end+1:]))
	if err != nil {
		return nil, err
	}
	return &Query{Matchers: matchers, Filters: filters}, nil
}

// closingBrace returns the index of the selector's closing brace, ignoring
// braces inside quoted values (regex quantifiers like [0-9]{2} contain one).
func closingBrace(s string) (int, error) {
	inQuote := false
	for i := 1; i < len(s); i++ {
		switch {
		case inQuote && s[i] == '\\':
			i++
		case s[i] == '"':
			inQuote = !inQuote
		case s[i] == '}' && !inQuote:
			return i, nil
		}
	}
	// Distinguish a missing quote from a missing brace so the reader gets the right fix.
	if inQuote {
		return 0, errors.New("logql: unterminated string in stream selector")
	}
	return 0, errors.New("logql: unclosed stream selector")
}

func parseMatchers(body string) ([]LabelMatcher, error) {
	var out []LabelMatcher
	rest := strings.TrimSpace(body)
	for rest != "" {
		label, r, err := scanIdent(rest)
		if err != nil {
			return nil, err
		}
		op, r, err := scanMatchOp(strings.TrimSpace(r))
		if err != nil {
			return nil, err
		}
		value, r, err := scanString(strings.TrimSpace(r))
		if err != nil {
			return nil, err
		}

		m := LabelMatcher{Label: label, Operator: op, Value: value}
		if op == MatchRegexp || op == MatchNotRegexp {
			// Loki anchors label-matcher regexes; match that.
			re, err := regexp.Compile("^(?:" + value + ")$")
			if err != nil {
				return nil, fmt.Errorf("logql: invalid regexp %q: %w", value, err)
			}
			m.Regexp = re
		}
		out = append(out, m)

		rest = strings.TrimSpace(r)
		if strings.HasPrefix(rest, ",") {
			rest = strings.TrimSpace(rest[1:])
			continue
		}
		if rest != "" {
			return nil, fmt.Errorf("logql: unexpected %q in stream selector", rest)
		}
	}
	return out, nil
}

func parseFilters(rest string) ([]LineFilter, error) {
	var out []LineFilter
	for rest != "" {
		if err := rejectUnsupported(rest); err != nil {
			return nil, err
		}
		op, r, err := scanFilterOp(rest)
		if err != nil {
			return nil, err
		}
		value, r, err := scanString(strings.TrimSpace(r))
		if err != nil {
			return nil, err
		}

		f := LineFilter{Operator: op, Value: value}
		if op == LineMatchesRegexp || op == LineNotMatchRegexp {
			re, err := regexp.Compile(value)
			if err != nil {
				return nil, fmt.Errorf("logql: invalid regexp %q: %w", value, err)
			}
			f.Regexp = re
		}
		out = append(out, f)
		rest = strings.TrimSpace(r)
	}
	return out, nil
}

func rejectUnsupported(rest string) error {
	if strings.HasPrefix(rest, "[") {
		return errors.New("logql: range vectors are not supported")
	}
	if !strings.HasPrefix(rest, "|") {
		return nil
	}
	tail := strings.TrimSpace(rest[1:])
	for _, name := range unsupported {
		if tail == name || strings.HasPrefix(tail, name+" ") || strings.HasPrefix(tail, name+"=") {
			return fmt.Errorf("logql: %q is not supported", name)
		}
	}
	return nil
}

func scanIdent(s string) (string, string, error) {
	i := 0
	for i < len(s) && (s[i] == '_' ||
		(s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') ||
		(i > 0 && s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 {
		return "", "", fmt.Errorf("logql: expected a label name, got %q", s)
	}
	return s[:i], s[i:], nil
}

func scanMatchOp(s string) (MatchOperator, string, error) {
	for _, op := range []MatchOperator{MatchRegexp, MatchNotRegexp, MatchNotEqual, MatchEqual} {
		if strings.HasPrefix(s, string(op)) {
			return op, s[len(op):], nil
		}
	}
	return "", "", fmt.Errorf("logql: expected =, !=, =~ or !~, got %q", s)
}

func scanFilterOp(s string) (LineFilterOperator, string, error) {
	for _, op := range []LineFilterOperator{LineContains, LineMatchesRegexp, LineNotContains, LineNotMatchRegexp} {
		if strings.HasPrefix(s, string(op)) {
			return op, s[len(op):], nil
		}
	}
	return "", "", fmt.Errorf("logql: unexpected %q after stream selector", s)
}

func scanString(s string) (string, string, error) {
	quoted, err := strconv.QuotedPrefix(s)
	if err != nil {
		return "", "", fmt.Errorf("logql: unterminated string at %q", s)
	}
	value, err := strconv.Unquote(quoted)
	if err != nil {
		return "", "", fmt.Errorf("logql: invalid string %s: %w", quoted, err)
	}
	return value, s[len(quoted):], nil
}
