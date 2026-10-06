// SPDX-License-Identifier: AGPL-3.0-only

package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.miloapis.com/telemetry/cli/logs"
)

const (
	defaultSince = time.Hour
	defaultLimit = 100
)

type logsOptions struct {
	since  string
	start  string
	end    string
	limit  int
	follow bool
	labels bool
	utc    bool
}

func logsCommand(e *env) *cobra.Command {
	var o logsOptions
	cmd := &cobra.Command{
		Use:   "logs QUERY",
		Short: "Query logs for the active project",
		Long: `Query logs for the active project, oldest line first.

A query is a stream selector with at least one label matcher, optionally
followed by line filters:

    {service_name="checkout"} |= "error" != "healthz"

Label matchers take =, !=, =~ and !~; line filters take |=, !=, |~ and !~.
Parsers, formatting stages and metric queries are not supported. Run
'datumctl telemetry labels' to see which labels you can match on.

By default the newest 100 lines from the last hour are shown. --limit -1 shows
every line in the window.

--follow keeps polling for new lines after the backlog. There is no push-based
streaming yet, so lines appear up to a couple of seconds late.

Logs are scoped to the project on the server; you can only read your own.`,
		Example: `  # The newest 100 lines from the last hour
  datumctl telemetry logs '{service_name="checkout"}'

  # Lines containing "error" over the last day
  datumctl telemetry logs '{service_name="checkout"} |= "error"' --since 24h --limit -1

  # An explicit window
  datumctl telemetry logs '{service_name="checkout"}' --start 2026-10-06T09:00:00Z --end 2026-10-06T10:00:00Z

  # Keep printing new lines
  datumctl telemetry logs '{service_name="checkout"}' -f

  # One JSON object per line
  datumctl telemetry logs '{service_name="checkout"}' -o json | jq .line`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, e, strings.TrimSpace(strings.Join(args, " ")), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.since, "since", "", "Only lines newer than this, e.g. 15m, 2h, 7d (default 1h)")
	f.StringVar(&o.start, "start", "", "Only lines at or after this RFC3339 time or unix seconds")
	f.StringVar(&o.end, "end", "", "Only lines before this RFC3339 time or unix seconds (default now)")
	f.IntVar(&o.limit, "limit", defaultLimit, "Newest lines to show, -1 for every line in the window")
	f.BoolVarP(&o.follow, "follow", "f", false, "Keep printing new lines as they arrive")
	f.BoolVar(&o.labels, "labels", false, "Prefix each line with its label set")
	f.BoolVar(&o.utc, "utc", false, "Show timestamps in UTC")
	return cmd
}

func runLogs(cmd *cobra.Command, e *env, query string, o logsOptions) error {
	if query == "" {
		return &UserError{Msg: "a log query is required", Hint: `start with a stream selector, e.g. '{service_name="checkout"}'`}
	}
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	if o.limit < -1 {
		return &UserError{Msg: "--limit must be -1 or greater"}
	}
	if o.follow && o.end != "" {
		return &UserError{Msg: "--end cannot be combined with --follow, which always reads up to now"}
	}
	start, end, err := resolveWindow(e.now(), o.since, o.start, o.end)
	if err != nil {
		return err
	}

	api, err := e.client(project(cmd))
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	p := &printer{w: e.streams.Out, json: format == "json", labels: o.labels, utc: o.utc}

	f := logs.Follower{Querier: api, Query: query, Status: e.streams.ErrOut}
	emit := p.emit
	if o.follow {
		// The backlog seeds the follower so the first poll doesn't reprint it.
		emit = func(en logs.Entry) {
			f.Backlog(en)
			p.emit(en)
		}
	}

	if o.limit >= 0 {
		entries, err := logs.Tail(ctx, api, query, start, end, o.limit)
		if err != nil {
			return err
		}
		for _, en := range entries {
			emit(en)
		}
	} else if err := logs.All(ctx, api, query, start, end, emit); err != nil {
		return err
	}
	if p.err != nil {
		return p.err
	}

	if !o.follow {
		if p.printed == 0 && o.limit != 0 {
			_, _ = fmt.Fprintf(e.streams.ErrOut, "No lines matched between %s and %s.\n",
				start.Format(time.RFC3339), end.Format(time.RFC3339))
			if empty := emptyLabels(ctx, api, query, start, end); len(empty) > 0 {
				verb := "has"
				if len(empty) > 1 {
					verb = "have"
				}
				_, _ = fmt.Fprintf(e.streams.ErrOut, "%s %s no values in that window; run '%s' to see which labels are set.\n",
					strings.Join(empty, ", "), verb, labelsCommandFor(cmd, o))
			}
		}
		return nil
	}
	if err := f.Run(ctx, p.emit); err != nil {
		return err
	}
	return p.err
}

// resolveWindow turns the window flags into [start, end).
func resolveWindow(now time.Time, since, start, end string) (time.Time, time.Time, error) {
	// --start and --since both set the lower bound. Honouring one and dropping
	// the other would return a window the caller did not ask for.
	if since != "" && start != "" {
		return time.Time{}, time.Time{}, &UserError{Msg: "--since and --start are mutually exclusive; pass only one"}
	}
	e := now
	if end != "" {
		t, err := parseTime("--end", end)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		e = t
	}
	var s time.Time
	switch {
	case start != "":
		t, err := parseTime("--start", start)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		s = t
	case since != "":
		d, err := parseSince(since)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		s = e.Add(-d)
	default:
		s = e.Add(-defaultSince)
	}
	if !s.Before(e) {
		return time.Time{}, time.Time{}, &UserError{Msg: "the window is empty: its start is not before its end"}
	}
	return s, e, nil
}

// parseSince accepts Go durations plus a day suffix, e.g. 7d.
func parseSince(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	} else if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, nil
	}
	return 0, &UserError{Msg: fmt.Sprintf("invalid --since %q", s), Hint: "use a positive duration such as 15m, 2h or 7d"}
}

func parseTime(flag, s string) (time.Time, error) {
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, &UserError{Msg: fmt.Sprintf("invalid %s %q", flag, s), Hint: "use an RFC3339 time such as 2026-10-06T09:00:00Z, or unix seconds"}
}

// printer renders entries as text or newline-delimited JSON.
type printer struct {
	w      io.Writer
	json   bool
	labels bool
	utc    bool

	printed int
	err     error // first write error; later writes are skipped
}

func (p *printer) emit(e logs.Entry) {
	if p.err != nil {
		return
	}
	p.printed++
	if p.json {
		p.err = json.NewEncoder(p.w).Encode(e)
		return
	}
	ts := e.Time
	if p.utc {
		ts = ts.UTC()
	} else {
		ts = ts.Local()
	}
	var b strings.Builder
	b.WriteString(ts.Format("2006-01-02T15:04:05.000Z07:00"))
	// Some lines carry everything in their labels and have no body, such as
	// ALB access logs. Printing just a timestamp would hide them.
	if p.labels || e.Line == "" {
		b.WriteString(" " + formatLabels(e.Labels))
	}
	if e.Line != "" {
		b.WriteString(" " + e.Line)
	}
	b.WriteString("\n")
	_, p.err = io.WriteString(p.w, b.String())
}

// formatLabels renders labels in sorted key order, so identical queries
// print identical text.
func formatLabels(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, k+"="+strconv.Quote(labels[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// labelsCommandFor is the labels command covering the same project and window
// as the logs command that found nothing. Without the window it would look at
// the default hour instead, and could contradict the hint that sent the user
// there.
func labelsCommandFor(cmd *cobra.Command, o logsOptions) string {
	args := []string{"datumctl", "telemetry", "labels"}
	for _, f := range []struct{ name, value string }{{"since", o.since}, {"start", o.start}, {"end", o.end}} {
		if f.value != "" {
			args = append(args, "--"+f.name, f.value)
		}
	}
	if cmd.Flags().Changed("project") {
		args = append(args, "--project", project(cmd))
	}
	return strings.Join(args, " ")
}

// emptyLabels returns the labels query requires (= or =~) that have no values
// in [start, end), so an empty result can say which matcher could never hold.
// It is best effort: a failed lookup is not reported.
func emptyLabels(ctx context.Context, api API, query string, start, end time.Time) []string {
	var empty []string
	for _, label := range requiredLabels(query) {
		values, err := api.LabelValues(ctx, label, start, end)
		if err == nil && len(values) == 0 {
			empty = append(empty, label)
		}
	}
	return empty
}

// matcherPattern finds a label name and its operator, in a selector whose
// quoted values have been blanked out.
var matcherPattern = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(=~|!~|!=|=)`)

// requiredLabels returns, in order and without repeats, the labels the
// query's stream selector matches positively. A label absent from a stream
// still satisfies != and !~, so those are left out.
func requiredLabels(query string) []string {
	var labels []string
	for _, m := range matcherPattern.FindAllStringSubmatch(unquotedSelector(query), -1) {
		if (m[2] == "=" || m[2] == "=~") && !slices.Contains(labels, m[1]) {
			labels = append(labels, m[1])
		}
	}
	return labels
}

// unquotedSelector returns the text between the selector's braces with every
// quoted string removed, so a value such as "a=b" is not read as a matcher.
func unquotedSelector(query string) string {
	open := strings.IndexByte(query, '{')
	if open < 0 {
		return ""
	}
	var b strings.Builder
	inQuote := false
	for i := open + 1; i < len(query); i++ {
		c := query[i]
		switch {
		case inQuote && c == '\\':
			i++ // skip the escaped character
		case c == '"':
			inQuote = !inQuote
			b.WriteByte(' ')
		case inQuote:
		case c == '}':
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
