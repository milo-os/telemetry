// SPDX-License-Identifier: AGPL-3.0-only

package prometheus_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
	"go.datum.net/o11y/queryapi/internal/storage/prometheus"
)

const label = "resourcemanager_datumapis_com_project_name"

type seen struct {
	method, path string
	form         url.Values
}

// backend answers every request with status and body, recording each one.
func backend(t *testing.T, status int, body string) (*httptest.Server, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var reqs []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("backend parse form: %v", err)
		}
		mu.Lock()
		reqs = append(reqs, seen{r.Method, r.URL.Path, r.PostForm})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seen { mu.Lock(); defer mu.Unlock(); return slices.Clone(reqs) }
}

func newStore(t *testing.T, url string) *prometheus.Store {
	t.Helper()
	s, err := prometheus.New(prometheus.Config{
		URL: url, Tenancy: prometheus.TenancyExtraLabel, ProjectLabel: label, MaxResponseBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

var projCtx = miloauth.WithProject(context.Background(), "proj-a")

const ok = `{"status":"success","data":{"resultType":"vector","result":[]}}`

func keys(v url.Values) []string {
	var ks []string
	for k := range v {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// Every call carries exactly one extra_label for the caller's project, and
// only the parameters the store built itself.
func TestEveryCallIsConfinedToTheProject(t *testing.T) {
	srv, reqs := backend(t, http.StatusOK, ok)
	s := newStore(t, srv.URL)
	tr := storage.TimeRange{Start: time.Unix(0, 0), End: time.Unix(60, 0)}

	// The query text tries to smuggle its own extra_label; it must arrive as
	// part of the query value, not as a parameter.
	smuggle := `up&extra_label=` + label + `=proj-b&extra_filters[]={x="y"}`
	calls := []struct {
		name, path string
		keys       []string
		call       func() error
	}{
		{"query", "/api/v1/query", []string{"extra_label", "query", "time", "timeout"}, func() error {
			_, err := s.Query(projCtx, storage.InstantQuery{Query: smuggle, Time: time.Unix(1, 0), Timeout: time.Second})
			return err
		}},
		{"query_range", "/api/v1/query_range", []string{"end", "extra_label", "query", "start", "step"}, func() error {
			_, err := s.QueryRange(projCtx, storage.RangeQuery{Query: smuggle, Range: tr, Step: time.Second})
			return err
		}},
		{"series", "/api/v1/series", []string{"end", "extra_label", "match[]", "start"}, func() error {
			_, err := s.Series(projCtx, []string{"up"}, tr)
			return err
		}},
		{"labels", "/api/v1/labels", []string{"extra_label"}, func() error {
			_, err := s.LabelNames(projCtx, nil, storage.TimeRange{})
			return err
		}},
		{"label values", "/api/v1/label/__name__/values", []string{"extra_label", "match[]"}, func() error {
			_, err := s.LabelValues(projCtx, "__name__", []string{"up"}, storage.TimeRange{})
			return err
		}},
	}
	for i, c := range calls {
		if err := c.call(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := reqs()[i]
		if got.method != http.MethodPost || got.path != c.path {
			t.Errorf("%s: %s %s, want POST %s", c.name, got.method, got.path, c.path)
		}
		if el := got.form["extra_label"]; len(el) != 1 || el[0] != label+"=proj-a" {
			t.Errorf("%s: extra_label = %q, want exactly [%s=proj-a]", c.name, el, label)
		}
		if k := keys(got.form); !slices.Equal(k, c.keys) {
			t.Errorf("%s: parameters = %v, want %v", c.name, k, c.keys)
		}
		if strings.HasPrefix(c.name, "query") && got.form.Get("query") != smuggle {
			t.Errorf("%s: query = %q, want the smuggled text verbatim", c.name, got.form.Get("query"))
		}
	}
}

func TestNoProjectSendsNothing(t *testing.T) {
	srv, reqs := backend(t, http.StatusOK, ok)
	s := newStore(t, srv.URL)
	if _, err := s.Query(context.Background(), storage.InstantQuery{Query: "up"}); !errors.Is(err, storage.ErrNoProject) {
		t.Fatalf("err = %v, want ErrNoProject", err)
	}
	if n := len(reqs()); n != 0 {
		t.Fatalf("backend received %d requests, want 0", n)
	}
}

func TestBackendResponses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"success", 200, ok, func(err error) bool { return err == nil }},
		{"bad query (422)", 422, `{"status":"error","errorType":"422","error":"unknown func foo"}`, func(err error) bool {
			var qe *storage.QueryError
			return errors.As(err, &qe) && qe.Message == "unknown func foo"
		}},
		{"bad query (400)", 400, `{"status":"error","errorType":"bad_data","error":"parse error"}`, func(err error) bool {
			var qe *storage.QueryError
			return errors.As(err, &qe)
		}},
		// Review focus 3: a proxy's HTML is not a query error.
		{"html 400", 400, `<html>bad request</html>`, func(err error) bool { return errors.Is(err, storage.ErrBackendUnavailable) }},
		{"html 502", 502, `<html>bad gateway</html>`, func(err error) bool { return errors.Is(err, storage.ErrBackendUnavailable) }},
		{"timeout", 503, `{"status":"error","errorType":"timeout","error":"query timed out"}`, func(err error) bool {
			return errors.Is(err, context.DeadlineExceeded)
		}},
		{"200 error envelope", 200, `{"status":"error","error":"?"}`, func(err error) bool { return errors.Is(err, storage.ErrBackendUnavailable) }},
		{"too large", 200, `{"status":"success","data":"` + strings.Repeat("x", 1<<20) + `"}`, func(err error) bool {
			return errors.Is(err, storage.ErrResponseTooLarge)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := backend(t, tc.status, tc.body)
			_, err := newStore(t, srv.URL).Query(projCtx, storage.InstantQuery{Query: "up"})
			if !tc.check(err) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestUnreachableAndDeadline(t *testing.T) {
	srv, _ := backend(t, 200, ok)
	addr := srv.URL
	srv.Close()
	if _, err := newStore(t, addr).Query(projCtx, storage.InstantQuery{Query: "up"}); !errors.Is(err, storage.ErrBackendUnavailable) {
		t.Errorf("unreachable: err = %v, want ErrBackendUnavailable", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server only notices a closed connection once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(projCtx, 50*time.Millisecond)
	defer cancel()
	if _, err := newStore(t, slow.URL).Query(ctx, storage.InstantQuery{Query: "up"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: err = %v, want DeadlineExceeded", err)
	}
}

// Review focus 5: long queries travel in the body intact.
func TestLongQueryArrivesIntact(t *testing.T) {
	srv, reqs := backend(t, 200, ok)
	q := "sum(" + strings.Repeat(`up{job="a"} + `, 1200) + "up)"
	if len(q) < 16<<10 {
		t.Fatalf("test query is only %d bytes", len(q))
	}
	if _, err := newStore(t, srv.URL).Query(projCtx, storage.InstantQuery{Query: q}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := reqs()[0].form.Get("query"); got != q {
		t.Errorf("query arrived as %d bytes, want %d", len(got), len(q))
	}
}

// Review focus 4: a label name that isn't one never reaches the backend.
func TestInvalidLabelNameSendsNothing(t *testing.T) {
	srv, reqs := backend(t, 200, ok)
	_, err := newStore(t, srv.URL).LabelValues(projCtx, "a/b", nil, storage.TimeRange{})
	var qe *storage.QueryError
	if !errors.As(err, &qe) || len(reqs()) != 0 {
		t.Errorf("err = %v, requests = %d; want a QueryError and none", err, len(reqs()))
	}
}

func TestBaseURLPathIsKept(t *testing.T) {
	srv, reqs := backend(t, 200, ok)
	if _, err := newStore(t, srv.URL+"/select/0/prometheus").Query(projCtx, storage.InstantQuery{Query: "up"}); err != nil {
		t.Fatal(err)
	}
	if p := reqs()[0].path; p != "/select/0/prometheus/api/v1/query" {
		t.Errorf("path = %s", p)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	good := prometheus.Config{URL: "http://vm:8429", Tenancy: prometheus.TenancyExtraLabel, ProjectLabel: label, MaxResponseBytes: 1}
	bad := map[string]func(*prometheus.Config){
		"no tenancy":      func(c *prometheus.Config) { c.Tenancy = "" },
		"unknown tenancy": func(c *prometheus.Config) { c.Tenancy = "rewrite" },
		"no url":          func(c *prometheus.Config) { c.URL = "" },
		"relative url":    func(c *prometheus.Config) { c.URL = "vm:8429" },
		"bad label":       func(c *prometheus.Config) { c.ProjectLabel = "project-name" },
		"zero size limit": func(c *prometheus.Config) { c.MaxResponseBytes = 0 },
	}
	if _, err := prometheus.New(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for name, mutate := range bad {
		c := good
		mutate(&c)
		if _, err := prometheus.New(c); err == nil {
			t.Errorf("%s: New succeeded, want an error", name)
		}
	}
}

func TestPing(t *testing.T) {
	healthy, reqs := backend(t, 200, "OK")
	if err := newStore(t, healthy.URL).Ping(context.Background()); err != nil {
		t.Errorf("healthy: %v", err)
	}
	if r := reqs(); r[0].method != http.MethodGet || r[0].path != "/-/healthy" {
		t.Errorf("ping sent %s %s", r[0].method, r[0].path)
	}
	sick, _ := backend(t, 500, "")
	if err := newStore(t, sick.URL).Ping(context.Background()); err == nil {
		t.Error("unhealthy backend: Ping = nil")
	}
}
