// SPDX-License-Identifier: AGPL-3.0-only

package queryapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.datum.net/o11y/queryapi"
	"go.datum.net/o11y/queryapi/internal/storage"
	"go.datum.net/o11y/queryapi/internal/storage/fake"
)

// recordingMetrics records what each handler asked the store for and answers
// with data or err.
type recordingMetrics struct {
	mu       sync.Mutex
	data     json.RawMessage
	err      error
	instant  storage.InstantQuery
	rng      storage.RangeQuery
	matchers []string
	name     string
	calls    int
}

func (m *recordingMetrics) record(f func()) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	f()
	if m.err != nil {
		return nil, m.err
	}
	if m.data == nil {
		return json.RawMessage(`[]`), nil
	}
	return m.data, nil
}

func (m *recordingMetrics) Query(_ context.Context, q storage.InstantQuery) (json.RawMessage, error) {
	return m.record(func() { m.instant = q })
}

func (m *recordingMetrics) QueryRange(_ context.Context, q storage.RangeQuery) (json.RawMessage, error) {
	return m.record(func() { m.rng = q })
}

func (m *recordingMetrics) Series(_ context.Context, ms []string, _ storage.TimeRange) (json.RawMessage, error) {
	return m.record(func() { m.matchers = ms })
}

func (m *recordingMetrics) LabelNames(_ context.Context, ms []string, _ storage.TimeRange) (json.RawMessage, error) {
	return m.record(func() { m.matchers = ms })
}

func (m *recordingMetrics) LabelValues(_ context.Context, name string, ms []string, _ storage.TimeRange) (json.RawMessage, error) {
	return m.record(func() { m.name, m.matchers = name, ms })
}

func (m *recordingMetrics) Ping(context.Context) error { return nil }

type promReply struct {
	Status    string          `json:"status"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

// promDo sends method to path with form as the body (POST) or the query
// string (GET), as an identified caller in proj-abc.
func promDo(t *testing.T, base, method, path string, form url.Values) (int, promReply) {
	t.Helper()
	target := base + prefix + path
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	} else {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	forwardIdentity(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close body: %v", err)
		}
	}()
	var r promReply
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return resp.StatusCode, r
}

func newPromServer(t *testing.T, m storage.MetricStore) string {
	t.Helper()
	cfg := queryapi.DefaultConfig()
	cfg.QueryTimeout = 30 * time.Second
	return serveWithMetrics(t, fake.New(2), m, cfg, allowAll()).URL
}

func TestPromQueryParameters(t *testing.T) {
	m := &recordingMetrics{}
	base := newPromServer(t, m)

	code, r := promDo(t, base, http.MethodPost, "/metrics/api/v1/query",
		url.Values{"query": {"up"}, "time": {"1760400000.123"}, "timeout": {"10s"}})
	if code != http.StatusOK || r.Status != "success" {
		t.Fatalf("status %d %+v", code, r)
	}
	if m.instant.Query != "up" || !m.instant.Time.Equal(time.Unix(1760400000, 123_000_000)) || m.instant.Timeout != 10*time.Second {
		t.Errorf("store got %+v", m.instant)
	}

	// A client timeout above --query-timeout is capped, not rejected.
	promDo(t, base, http.MethodPost, "/metrics/api/v1/query", url.Values{"query": {"up"}, "timeout": {"5m"}})
	if m.instant.Timeout != 30*time.Second {
		t.Errorf("timeout = %v, want capped at 30s", m.instant.Timeout)
	}
}

func TestPromBadRequests(t *testing.T) {
	m := &recordingMetrics{}
	base := newPromServer(t, m)
	rangeForm := func(start, end, step string) url.Values {
		return url.Values{"query": {"up"}, "start": {start}, "end": {end}, "step": {step}}
	}
	cases := []struct {
		name, method, path string
		form               url.Values
	}{
		{"query missing", http.MethodPost, "/metrics/api/v1/query", url.Values{}},
		{"bad time", http.MethodPost, "/metrics/api/v1/query", url.Values{"query": {"up"}, "time": {"soon"}}},
		{"zero timeout", http.MethodPost, "/metrics/api/v1/query", url.Values{"query": {"up"}, "timeout": {"0"}}},
		{"range no start", http.MethodPost, "/metrics/api/v1/query_range", rangeForm("", "60", "1")},
		{"range no end", http.MethodPost, "/metrics/api/v1/query_range", rangeForm("0", "", "1")},
		{"range no step", http.MethodPost, "/metrics/api/v1/query_range", rangeForm("0", "60", "")},
		{"range end before start", http.MethodPost, "/metrics/api/v1/query_range", rangeForm("60", "0", "1")},
		{"range over max-range", http.MethodPost, "/metrics/api/v1/query_range", rangeForm("0", "2678401", "3600")},
		{"range over 11000 points", http.MethodPost, "/metrics/api/v1/query_range", rangeForm("0", "11000", "1")},
		{"series no match", http.MethodGet, "/metrics/api/v1/series", url.Values{}},
		{"label name with digit", http.MethodGet, "/metrics/api/v1/label/1abc/values", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := m.calls
			code, r := promDo(t, base, tc.method, tc.path, tc.form)
			if code != http.StatusBadRequest || r.ErrorType != "bad_data" {
				t.Errorf("got %d %+v, want 400 bad_data", code, r)
			}
			if m.calls != before {
				t.Errorf("store was called for a request that failed validation")
			}
		})
	}
}

// Review focus 4: an escaped slash decodes to "a/b" in the path value. The
// apiserver's filters may refuse it before the handler does, so any 4xx
// passes; reaching the store does not.
func TestPromLabelNameWithSlashNeverReachesTheStore(t *testing.T) {
	m := &recordingMetrics{}
	base := newPromServer(t, m)
	req, err := http.NewRequest(http.MethodGet, base+prefix+"/metrics/api/v1/label/a%2Fb/values", nil)
	if err != nil {
		t.Fatal(err)
	}
	forwardIdentity(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("close body: %v", err)
	}
	if resp.StatusCode < 400 || resp.StatusCode >= 500 || m.calls != 0 {
		t.Errorf("status %d, store calls %d; want a 4xx and none", resp.StatusCode, m.calls)
	}
}

func TestPromRangeAtTheLimitsIsAccepted(t *testing.T) {
	m := &recordingMetrics{}
	base := newPromServer(t, m)
	// 0..10999 at 1s is exactly 11,000 points.
	code, r := promDo(t, base, http.MethodPost, "/metrics/api/v1/query_range",
		url.Values{"query": {"up"}, "start": {"0"}, "end": {"10999"}, "step": {"1"}})
	if code != http.StatusOK {
		t.Fatalf("got %d %+v", code, r)
	}
	if m.rng.Step != time.Second || !m.rng.Range.End.Equal(time.Unix(10999, 0)) {
		t.Errorf("store got %+v", m.rng)
	}
}

func TestPromSelectorsAndLabelNames(t *testing.T) {
	m := &recordingMetrics{}
	base := newPromServer(t, m)

	promDo(t, base, http.MethodGet, "/metrics/api/v1/series", url.Values{"match[]": {"up", `{job="a"}`}})
	if len(m.matchers) != 2 || m.matchers[0] != "up" || m.matchers[1] != `{job="a"}` {
		t.Errorf("series matchers = %q", m.matchers)
	}

	code, _ := promDo(t, base, http.MethodGet, "/metrics/api/v1/label/__name__/values", nil)
	if code != http.StatusOK || m.name != "__name__" {
		t.Errorf("label values: %d, name %q", code, m.name)
	}
}

// Review focus 1: scalar, string and matrix results pass through unchanged.
func TestPromDataPassesThroughUnchanged(t *testing.T) {
	for _, data := range []string{
		`{"resultType":"scalar","result":[1760400000,"2"]}`,
		`{"resultType":"string","result":[1760400000,"hello"]}`,
		`{"resultType":"matrix","result":[]}`,
	} {
		m := &recordingMetrics{data: json.RawMessage(data)}
		base := newPromServer(t, m)
		_, r := promDo(t, base, http.MethodPost, "/metrics/api/v1/query", url.Values{"query": {"1+1"}})
		if string(r.Data) != data {
			t.Errorf("data = %s, want %s", r.Data, data)
		}
	}
}

func TestPromStorageErrors(t *testing.T) {
	cases := []struct {
		err       error
		code      int
		errorType string
	}{
		{&storage.QueryError{Message: "unknown function foo"}, http.StatusBadRequest, "bad_data"},
		{storage.ErrNoProject, http.StatusUnauthorized, "unauthorized"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, "timeout"},
		{storage.ErrResponseTooLarge, http.StatusUnprocessableEntity, "execution"},
		{fmt.Errorf("%w: dial tcp 10.0.0.1:8429", storage.ErrBackendUnavailable), http.StatusBadGateway, "unavailable"},
		{errors.New("boom"), http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		base := newPromServer(t, &recordingMetrics{err: tc.err})
		code, r := promDo(t, base, http.MethodPost, "/metrics/api/v1/query", url.Values{"query": {"up"}})
		if code != tc.code || r.ErrorType != tc.errorType {
			t.Errorf("%v: got %d %q, want %d %q", tc.err, code, r.ErrorType, tc.code, tc.errorType)
		}
		if strings.Contains(r.Error, "10.0.0.1") {
			t.Errorf("%v: internal address leaked to the client: %q", tc.err, r.Error)
		}
	}
	// The backend's own message for a bad query is returned.
	base := newPromServer(t, &recordingMetrics{err: &storage.QueryError{Message: "unknown function foo"}})
	if _, r := promDo(t, base, http.MethodPost, "/metrics/api/v1/query", url.Values{"query": {"foo()"}}); r.Error != "unknown function foo" {
		t.Errorf("error = %q", r.Error)
	}
}
