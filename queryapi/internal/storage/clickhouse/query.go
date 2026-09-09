// SPDX-License-Identifier: AGPL-3.0-only

package clickhouse

import (
	"fmt"
	"strings"

	"go.datum.net/o11y/queryapi/internal/logql"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// logsSelect lists the columns rowIterator scans, in the same order. The three
// promoted columns and the two attribute maps build each row's label set (see
// assembleLabels). ObservedTimestamp is the query key (window, order, partition),
// frozen at Collector receipt per the 000001_init migration; Row.Timestamp is
// filled from it.
const logsSelect = "ObservedTimestamp, Body, ServiceName, SeverityText, TraceId, ResourceAttributes, LogAttributes"

// projectRange returns the WHERE fragments and args that scope a query to one
// project over the half-open interval [Start, End), enforced as a partitioned
// prefix. Everything else in the query layer appends to this.
func projectRange(project string, tr storage.TimeRange) (string, []any) {
	return "ProjectId = ? AND ObservedTimestamp >= ? AND ObservedTimestamp < ?", []any{project, tr.Start, tr.End}
}

// buildLogsQuery translates a parsed LogQL query into SELECT ... over the
// logs table with every constraint pushed down to SQL: project, time range,
// label matchers, line filters, ordering and limit. Values are always bound
// as parameters, and column names come only from the fixed allowlist in
// storage.Resolve.
func buildLogsQuery(project string, q storage.LogQuery) (string, []any, error) {
	if q.Limit <= 0 {
		return "", nil, storage.ErrInvalidLimit
	}

	where, args := projectRange(project, q.Range)
	for _, m := range q.Matchers {
		cond, margs := matcherFragment(m)
		where += " AND " + cond
		args = append(args, margs...)
	}
	for _, f := range q.Filters {
		cond, fargs := lineFilterFragment(f)
		where += " AND " + cond
		args = append(args, fargs...)
	}

	order := "ASC"
	if q.Direction == storage.DirectionBackward {
		order = "DESC"
	}

	query := fmt.Sprintf("SELECT %s FROM logs WHERE %s ORDER BY ObservedTimestamp %s LIMIT ?",
		logsSelect, where, order)
	args = append(args, q.Limit)
	return query, args, nil
}

// matcherFragment renders a label matcher as a WHERE condition. Regular
// expressions reuse the anchored *regexp.Regexp the parser compiled, keeping
// LogQL's anchoring semantics exact.
func matcherFragment(m logql.LabelMatcher) (string, []any) {
	expr, eargs := labelExpr(m.Label)
	switch m.Operator {
	case logql.MatchEqual:
		eargs = append(eargs, m.Value)
		return expr + " = ?", eargs
	case logql.MatchNotEqual:
		eargs = append(eargs, m.Value)
		return "(" + expr + " != ?)", eargs
	case logql.MatchRegexp:
		eargs = append(eargs, m.Regexp.String())
		return "match(" + expr + ", ?)", eargs
	case logql.MatchNotRegexp:
		eargs = append(eargs, m.Regexp.String())
		return "NOT match(" + expr + ", ?)", eargs
	default:
		return "1 = 1", eargs
	}
}

// lineFilterFragment renders a LogQL line filter against the Body column.
// position() is used for text containment so % and _ in the filter cannot be
// misread as LIKE wildcards; regexp filters stay unanchored like Loki's.
func lineFilterFragment(f logql.LineFilter) (string, []any) {
	switch f.Operator {
	case logql.LineContains:
		return "position(Body, ?) > 0", []any{f.Value}
	case logql.LineNotContains:
		return "position(Body, ?) = 0", []any{f.Value}
	case logql.LineMatchesRegexp:
		return "match(Body, ?)", []any{f.Regexp.String()}
	case logql.LineNotMatchRegexp:
		return "NOT match(Body, ?)", []any{f.Regexp.String()}
	default:
		return "1 = 1", nil
	}
}

// labelExpr resolves a LogQL label to the SQL expression that reads its value
// and any parameters it needs (map accesses bind the key). Known labels are
// the schema's promoted columns; everything else is an attribute-map lookup.
func labelExpr(label string) (string, []any) {
	target, kind := storage.Resolve(label)
	switch kind {
	case storage.LabelColumn:
		return target, nil
	case storage.LabelResourceAttribute:
		return "ResourceAttributes[?]", []any{target}
	case storage.LabelLogAttribute:
		return "LogAttributes[?]", []any{target}
	default:
		return target, nil
	}
}

// fixedSchemaLabels are the promoted columns openapi.yaml's /labels contract
// says are always present, whatever attribute keys data holds.
func fixedSchemaLabels() []string {
	return []string{"service_name", "severity"}
}

// buildLabelNamesQuery lists the distinct attribute keys in the maps within a
// project's window. The fixed schema columns are added by the store from
// fixedSchemaLabels, so they are not part of this query.
func buildLabelNamesQuery(project string, tr storage.TimeRange) (string, []any) {
	cond, prefix := projectRange(project, tr)

	var frags []string
	var args []any
	for _, mapCol := range []string{"ResourceAttributes", "LogAttributes"} {
		frags = append(frags, fmt.Sprintf(
			"SELECT arrayJoin(mapKeys(%s)) FROM logs WHERE %s", mapCol, cond))
		args = append(args, prefix...)
	}
	return strings.Join(frags, " UNION ALL "), args
}

// buildLabelValuesQuery lists the distinct values of one label within a
// project's window: a fixed column when the label is promoted, otherwise a
// map access, matching openapi.yaml's /label/{name}/values contract. Any map
// key parameter is bound before the WHERE args because its ? appears first in
// the SELECT text and bindings are positional.
func buildLabelValuesQuery(project, label string, tr storage.TimeRange) (string, []any) {
	expr, eargs := labelExpr(label)
	where, wargs := projectRange(project, tr)
	return fmt.Sprintf("SELECT DISTINCT %s FROM logs WHERE %s", expr, where), append(eargs, wargs...)
}

// seriesAllowlist bounds Series to a small, fixed combination so the distinct
// count query cannot explode into a cross product over per-record attribute
// keys. Matches openapi.yaml's /series contract.
var seriesAllowlist = []struct {
	label  string
	kind   storage.LabelKind
	target string
}{
	{label: "service_name", kind: storage.LabelColumn, target: "ServiceName"},
	{label: "severity", kind: storage.LabelColumn, target: "SeverityText"},
	{label: "resource_name", kind: storage.LabelResourceAttribute, target: "resource_name"},
}

// buildSeriesQuery lists the distinct bounded label-sets matching the
// selectors within a project's window. Matchers constrain which rows qualify
// but are not part of the returned set (limited to seriesAllowlist). Map-access
// keys are bound once for SELECT and once for GROUP BY, ahead of the WHERE
// args, matching the order their ?s appear.
func buildSeriesQuery(project string, matchers []logql.LabelMatcher, tr storage.TimeRange) (string, []any) {
	exprs := make([]string, len(seriesAllowlist))
	var selectArgs []any
	for i, slot := range seriesAllowlist {
		expr, args := seriesExpr(slot)
		exprs[i] = expr
		selectArgs = append(selectArgs, args...)
	}

	where, wargs := projectRange(project, tr)
	for _, m := range matchers {
		cond, margs := matcherFragment(m)
		where += " AND " + cond
		wargs = append(wargs, margs...)
	}

	// The GROUP BY text repeats the allowlist exprs verbatim, so its map-access
	// ?s need their own bindings after the WHERE's.
	var groupArgs []any
	for _, slot := range seriesAllowlist {
		_, args := seriesExpr(slot)
		groupArgs = append(groupArgs, args...)
	}

	groupBy := strings.Join(exprs, ", ")
	args := append(selectArgs, wargs...)
	args = append(args, groupArgs...)
	return fmt.Sprintf(
		"SELECT %s FROM logs WHERE %s GROUP BY %s",
		strings.Join(exprs, ", "), where, groupBy), args
}

// seriesExpr builds the SQL expression that reads an allowlisted slot from the
// row: a fixed column, or a map access bound to the slot's key.
func seriesExpr(slot struct {
	label  string
	kind   storage.LabelKind
	target string
}) (string, []any) {
	switch slot.kind {
	case storage.LabelColumn:
		return slot.target, nil
	case storage.LabelResourceAttribute:
		return "ResourceAttributes[?]", []any{slot.target}
	default:
		return "LogAttributes[?]", []any{slot.target}
	}
}
