// SPDX-License-Identifier: AGPL-3.0-only

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"go.miloapis.com/telemetry/cli/logs"
)

type fakeAPI struct {
	entries []logs.Entry
	names   []string
	values  map[string][]string
	err     error

	queries []logs.Query
	window  [2]time.Time // of the last label lookup
	onQuery func(n int)  // called after each query with the count so far
}

func (f *fakeAPI) QueryRange(_ context.Context, q logs.Query) ([]logs.Entry, error) {
	f.queries = append(f.queries, q)
	if f.onQuery != nil {
		defer f.onQuery(len(f.queries))
	}
	if f.err != nil {
		return nil, f.err
	}
	var out []logs.Entry
	for _, e := range f.entries {
		if !e.Time.Before(q.Start) && e.Time.Before(q.End) {
			out = append(out, e)
		}
	}
	logs.Sort(out, q.Direction)
	return out[:min(q.Limit, len(out))], nil
}

func (f *fakeAPI) LabelNames(_ context.Context, start, end time.Time) ([]string, error) {
	f.window = [2]time.Time{start, end}
	return f.names, f.err
}

func (f *fakeAPI) LabelValues(_ context.Context, label string, _, _ time.Time) ([]string, error) {
	return f.values[label], f.err
}

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func run(t *testing.T, api *fakeAPI, args ...string) (string, string, error) {
	t.Helper()
	return runCtx(t, context.Background(), api, args...)
}

func runCtx(t *testing.T, ctx context.Context, api *fakeAPI, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	var gotProject string
	root := newRoot(&env{
		streams: Streams{Out: &out, ErrOut: &errOut},
		client: func(project string) (API, error) {
			gotProject = project
			return api, nil
		},
		now: func() time.Time { return now },
	})
	root.SetArgs(append(args, "--project", "p-1"))
	err := root.ExecuteContext(ctx)
	if gotProject != "" && gotProject != "p-1" {
		t.Errorf("client built for project %q, want p-1", gotProject)
	}
	return out.String(), errOut.String(), err
}

func entry(ago time.Duration, line string, labels map[string]string) logs.Entry {
	return logs.Entry{Time: now.Add(-ago), Line: line, Labels: labels}
}

func TestLogsTailsOldestFirst(t *testing.T) {
	api := &fakeAPI{entries: []logs.Entry{
		entry(3*time.Minute, "three", map[string]string{"s": "a"}),
		entry(1*time.Minute, "one", map[string]string{"s": "b"}),
		entry(2*time.Minute, "two", map[string]string{"s": "a"}),
		entry(2*time.Hour, "outside the default hour", nil),
	}}
	out, _, err := run(t, api, "logs", `{s=~".+"}`, "--limit", "2", "--utc", "--labels")
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-10-06T11:58:00.000Z {s=\"a\"} two\n2026-10-06T11:59:00.000Z {s=\"b\"} one\n"
	if out != want {
		t.Errorf("output =\n%s\nwant\n%s", out, want)
	}
	if q := api.queries[0]; q.Query != `{s=~".+"}` || !q.Start.Equal(now.Add(-time.Hour)) || !q.End.Equal(now) {
		t.Errorf("query = %+v, want the selector over the last hour", q)
	}
}

// Access logs have no body; their labels are the whole record.
func TestLogsWithoutBodyShowLabels(t *testing.T) {
	api := &fakeAPI{entries: []logs.Entry{entry(time.Minute, "", map[string]string{"response_code": "404", "method": "GET"})}}
	out, _, err := run(t, api, "logs", `{response_code=~".+"}`, "--utc")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-10-06T11:59:00.000Z {method=\"GET\", response_code=\"404\"}\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestLogsJSON(t *testing.T) {
	api := &fakeAPI{entries: []logs.Entry{entry(time.Minute, "hello", map[string]string{"s": "a"})}}
	out, _, err := run(t, api, "logs", "{s=\"a\"}", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Time   time.Time         `json:"time"`
		Labels map[string]string `json:"labels"`
		Line   string            `json:"line"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q is not one JSON object: %v", out, err)
	}
	if got.Line != "hello" || got.Labels["s"] != "a" || !got.Time.Equal(now.Add(-time.Minute)) {
		t.Errorf("decoded %+v", got)
	}
}

func TestLogsEmptyResultSaysSo(t *testing.T) {
	out, errOut, err := run(t, &fakeAPI{}, "logs", "{s=\"a\"}")
	if err != nil || out != "" || !strings.Contains(errOut, "No lines matched") {
		t.Errorf("got out=%q errOut=%q err=%v", out, errOut, err)
	}
}

func TestLogsRejects(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"since and start", []string{"logs", "{a=\"1\"}", "--since", "1h", "--start", "2026-10-06T00:00:00Z"}, "mutually exclusive"},
		{"follow with end", []string{"logs", "{a=\"1\"}", "-f", "--end", "2026-10-06T00:00:00Z"}, "--follow"},
		{"bad since", []string{"logs", "{a=\"1\"}", "--since", "yesterday"}, "invalid --since"},
		{"bad start", []string{"logs", "{a=\"1\"}", "--start", "monday"}, "invalid --start"},
		{"empty window", []string{"logs", "{a=\"1\"}", "--start", "2026-10-06T12:00:00Z", "--end", "2026-10-06T11:00:00Z"}, "window is empty"},
		{"yaml output", []string{"logs", "{a=\"1\"}", "-o", "yaml"}, "unsupported --output"},
		{"bad limit", []string{"logs", "{a=\"1\"}", "--limit", "-2"}, "--limit"},
		{"no query", []string{"logs"}, "requires at least 1 arg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{}
			_, _, err := run(t, api, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
			if len(api.queries) != 0 {
				t.Errorf("queried the API %d times before rejecting the flags", len(api.queries))
			}
		})
	}
}

func TestRenderShowsServerReasonAndHint(t *testing.T) {
	api := &fakeAPI{err: &logs.APIError{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Detail: "denied by policy"}}
	_, _, err := run(t, api, "logs", "{a=\"1\"}")
	if err == nil {
		t.Fatal("want an error")
	}
	var buf bytes.Buffer
	Render(&buf, err)
	if s := buf.String(); !strings.Contains(s, "denied by policy") || !strings.Contains(s, "logs.query") {
		t.Errorf("Render() = %q, want the server's reason and the permission hint", s)
	}
}

func TestLabels(t *testing.T) {
	api := &fakeAPI{names: []string{"service_name", "level"}, values: map[string][]string{"service_name": {"web", "api"}}}

	out, _, err := run(t, api, "labels")
	if err != nil || out != "level\nservice_name\n" {
		t.Errorf("labels = %q, %v", out, err)
	}
	out, _, err = run(t, api, "labels", "service_name", "-o", "json")
	if err != nil || out != "[\"api\",\"web\"]\n" {
		t.Errorf("labels service_name -o json = %q, %v", out, err)
	}

	// labels takes the same window flags as logs, with the same checks.
	if _, _, err := run(t, api, "labels", "--start", "2026-10-06T00:00:00Z", "--end", "2026-10-06T01:00:00Z"); err != nil {
		t.Errorf("labels --start --end = %v", err)
	}
	if want := [2]time.Time{now.Add(-12 * time.Hour), now.Add(-11 * time.Hour)}; !api.window[0].Equal(want[0]) || !api.window[1].Equal(want[1]) {
		t.Errorf("labels looked up %v, want %v", api.window, want)
	}
	if _, _, err := run(t, api, "labels", "--since", "1h", "--start", "2026-10-06T00:00:00Z"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("labels --since --start = %v, want a refusal", err)
	}
}

func TestParseSince(t *testing.T) {
	for in, want := range map[string]time.Duration{"15m": 15 * time.Minute, "2h": 2 * time.Hour, "7d": 7 * 24 * time.Hour} {
		if got, err := parseSince(in); err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"0s", "-1h", "0d", "d", "1w"} {
		if _, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q) succeeded, want an error", in)
		}
	}
}

// The backlog's lines come back in the first poll's overlap; following must
// print only what is new. The follower polls up to the real clock, so this test
// does too.
func TestLogsFollowDoesNotRepeatBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	real := time.Now()
	api := &fakeAPI{entries: []logs.Entry{
		{Time: real.Add(-5 * time.Second), Line: "backlog"},
		{Time: real.Add(time.Second), Line: "new"}, // after the tail's end: only a poll reaches it
	}}
	api.onQuery = func(n int) {
		if n == 3 { // the tail, a poll that finds "new", then stop
			cancel()
		}
	}

	var out, errOut bytes.Buffer
	root := newRoot(&env{
		streams: Streams{Out: &out, ErrOut: &errOut},
		client:  func(string) (API, error) { return api, nil },
		now:     func() time.Time { return real },
	})
	root.SetArgs([]string{"logs", `{a="1"}`, "-f", "-o", "json"})
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	var lines []string
	dec := json.NewDecoder(&out)
	for dec.More() {
		var e logs.Entry
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, e.Line)
	}
	if got := strings.Join(lines, ","); got != "backlog,new" {
		t.Errorf("printed %s, want backlog,new", got)
	}
}

func TestRequiredLabels(t *testing.T) {
	tests := map[string][]string{
		`{service_name=~".+"}`:                          {"service_name"},
		`{a="1", b=~"x|y", c!="2", d!~"z"} |= "e=f"`:    {"a", "b"},
		`{a = "1",a="2"}`:                               {"a"},
		`{path="/x?a=b", method="GET"}`:                 {"path", "method"},
		`{msg="say \"k=v\" now", level="error"} != "x"`: {"msg", "level"},
		`not a selector`:                                nil,
	}
	for query, want := range tests {
		if got := requiredLabels(query); !slices.Equal(got, want) {
			t.Errorf("requiredLabels(%s) = %v, want %v", query, got, want)
		}
	}
}

// An empty result names the matchers that could never hold, so a query on an
// unset label is not mistaken for a quiet project.
func TestLogsEmptyResultNamesUnsetLabels(t *testing.T) {
	api := &fakeAPI{values: map[string][]string{"response_code": {"200"}}}
	_, errOut, err := run(t, api, "logs", `{service_name=~".+", response_code="200", status_code="500", gone!="x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "service_name, status_code have no values") || strings.Contains(errOut, "response_code have") || strings.Contains(errOut, "gone") {
		t.Errorf("errOut = %q, want only service_name and status_code named", errOut)
	}

	// The suggested command covers the same project and window, or it would
	// look at the default hour and could contradict the hint.
	_, errOut, _ = run(t, api, "logs", `{service_name=~".+"}`, "--since", "30d")
	if want := "run 'datumctl telemetry labels --since 30d --project p-1'"; !strings.Contains(errOut, want) {
		t.Errorf("errOut = %q, want it to suggest %q", errOut, want)
	}

	// Every required label is set: the base message alone.
	_, errOut, _ = run(t, api, "logs", `{response_code="200"}`)
	if strings.Contains(errOut, "no values") {
		t.Errorf("errOut = %q, want no label hint when every label is set", errOut)
	}
}
