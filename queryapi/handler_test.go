// SPDX-License-Identifier: AGPL-3.0-only

package queryapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"k8s.io/apiserver/pkg/authorization/authorizer"

	"go.datum.net/o11y/queryapi"
	"go.datum.net/o11y/queryapi/internal/logql"
	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
	"go.datum.net/o11y/queryapi/internal/storage/fake"
)

// discardLogger is the logger every test server is built with.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// specBase is a concrete instance of openapi.yaml's server template, which
// kube-aggregator fills in at runtime. Route matching against the spec needs
// one; the test server's own URL is not it.
const specBase = "/projects/test-project/control-plane/apis/o11y.miloapis.com/v1alpha1"

// loadSpec parses and validates openapi.yaml, then builds a router that maps
// requests to the documented operations.
func loadSpec(t *testing.T) routers.Router {
	t.Helper()

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatalf("load openapi.yaml: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml failed its own schema validation: %v", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("build router from openapi.yaml: %v", err)
	}
	return router
}

// TestHandlersConformToSpec drives every documented endpoint and validates the response against openapi.yaml.
func TestHandlersConformToSpec(t *testing.T) {
	router := loadSpec(t)

	cfg := queryapi.DefaultConfig()
	srv := serve(t, fake.New(2), cfg, allowAll())

	cases := []struct {
		name   string
		method string
		path   string
		query  string
		form   url.Values
	}{
		{"loki query", http.MethodGet, "/logs/loki/api/v1/query", `query={service_name="waf"}`, nil},
		{
			"loki query_range", http.MethodGet, "/logs/loki/api/v1/query_range",
			`query={service_name="waf"}&start=2026-01-01T00:00:00Z&end=2026-01-01T01:00:00Z`, nil,
		},
		{"loki label names", http.MethodGet, "/logs/loki/api/v1/labels", "", nil},
		{"loki label values", http.MethodGet, "/logs/loki/api/v1/label/severity/values", "", nil},
		{"loki series", http.MethodGet, "/logs/loki/api/v1/series", `match[]={service_name="foo"}`, nil},
		{"prom query", http.MethodPost, "/metrics/api/v1/query", "", url.Values{"query": {"cpu_usage"}}},
		{
			"prom query_range", http.MethodPost, "/metrics/api/v1/query_range", "",
			url.Values{"query": {"cpu_usage"}, "start": {"2026-01-01T00:00:00Z"}, "end": {"2026-01-01T01:00:00Z"}, "step": {"1m"}},
		},
		{"prom series", http.MethodGet, "/metrics/api/v1/series", `match[]=cpu_usage{service="foo"}`, nil},
		{"prom label names", http.MethodGet, "/metrics/api/v1/labels", "", nil},
		{"prom label values", http.MethodGet, "/metrics/api/v1/label/service/values", "", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			realURL := srv.URL + prefix + tc.path
			if tc.query != "" {
				realURL += "?" + tc.query
			}

			var body io.Reader
			if tc.form != nil {
				body = strings.NewReader(tc.form.Encode())
			}
			httpReq, err := http.NewRequest(tc.method, realURL, body)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tc.form != nil {
				httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			forwardIdentity(httpReq)

			resp, err := http.DefaultClient.Do(httpReq)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer func() {
				if err := resp.Body.Close(); err != nil {
					t.Logf("close response body: %v", err)
				}
			}()
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read response body: %v", err)
			}

			specURL := &url.URL{Path: specBase + tc.path, RawQuery: tc.query}
			var specBody io.Reader
			if tc.form != nil {
				specBody = strings.NewReader(tc.form.Encode())
			}
			specReq, err := http.NewRequest(tc.method, specURL.String(), specBody)
			if err != nil {
				t.Fatalf("build spec request: %v", err)
			}
			if tc.form != nil {
				specReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}

			route, pathParams, err := router.FindRoute(specReq)
			if err != nil {
				t.Fatalf("find route for %s: %v", tc.path, err)
			}

			requestValidationInput := &openapi3filter.RequestValidationInput{
				Request:    specReq,
				PathParams: pathParams,
				Route:      route,
			}
			responseValidationInput := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: requestValidationInput,
				Status:                 resp.StatusCode,
				Header:                 resp.Header,
				Body:                   io.NopCloser(bytes.NewReader(respBody)),
			}
			if err := openapi3filter.ValidateResponse(context.Background(), responseValidationInput); err != nil {
				t.Fatalf("response for %s did not conform to openapi.yaml: %v\nbody: %s", tc.path, err, respBody)
			}
		})
	}
}

// TestAnonymousRequestIsNotServed pins what an unauthenticated caller gets.
// The delegating authenticator admits system:anonymous rather than rejecting
// it -- that is how the kubelet reaches /healthz -- so an anonymous query is
// authenticated and then denied, which is a 403 rather than the 401 queryapi
// used to answer before it ran on the apiserver runtime.
func TestAnonymousRequestIsNotServed(t *testing.T) {
	store := &recordingStore{}
	srv := serve(t, store, queryapi.DefaultConfig(), allowAll())

	resp, err := http.Get(srv.URL + prefix + "/logs/loki/api/v1/query?query=%7Bservice_name%3D%22waf%22%7D")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := store.seen(); got != "" {
		t.Errorf("backend saw project %q, want none", got)
	}
}

// TestSeriesUnionsSelectors pins Loki's union semantics for repeated match[]:
// two selectors return both label sets, the same selector twice returns no
// duplicates, and a missing selector is a 400.
func TestSeriesUnionsSelectors(t *testing.T) {
	cfg := queryapi.DefaultConfig()
	srv := serve(t, fake.New(20), cfg, allowAll())

	get := func(t *testing.T, query string) (int, []map[string]string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+prefix+"/logs/loki/api/v1/series?"+query, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		forwardIdentity(req)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Logf("close response body: %v", err)
			}
		}()

		var body struct {
			Status string              `json:"status"`
			Data   []map[string]string `json:"data"`
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
		}
		return resp.StatusCode, body.Data
	}

	code, data := get(t, `match[]={service_name="waf"}&match[]={service_name="envoy-gateway"}`)
	if code != http.StatusOK {
		t.Fatalf("two selectors: status = %d, want 200", code)
	}
	services := map[string]bool{}
	for _, ls := range data {
		services[ls["service_name"]] = true
	}
	if !services["waf"] || !services["envoy-gateway"] {
		t.Errorf("two selectors returned services %v, want both waf and envoy-gateway", services)
	}

	_, dup := get(t, `match[]={service_name="waf"}&match[]={service_name="waf"}`)
	_, single := get(t, `match[]={service_name="waf"}`)
	if len(dup) != len(single) {
		t.Errorf("repeated identical selector returned %d series, want %d (deduped)", len(dup), len(single))
	}
	// > 1, not just > 0: with a single waf label set the dedupe comparison
	// above would also pass for a key coarser than LabelSet.Key(), which
	// would silently collapse distinct severity/resource combinations.
	if len(single) < 2 {
		t.Fatalf("single waf selector returned %d series, want at least 2 distinct label sets", len(single))
	}

	if code, _ := get(t, ""); code != http.StatusBadRequest {
		t.Errorf("no match[]: status = %d, want 400", code)
	}
}

// recordingStore records the project each query was scoped to, so a routing
// test can assert the identity that reached the backend.
type recordingStore struct {
	mu      sync.Mutex
	project string
}

func (s *recordingStore) note(ctx context.Context) error {
	id, ok := miloauth.ProjectID(ctx)
	if !ok {
		return storage.ErrNoProject
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.project = id
	return nil
}

func (s *recordingStore) seen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.project
}

func (s *recordingStore) QueryLogs(ctx context.Context, q storage.LogQuery) (storage.LogIterator, error) {
	if err := s.note(ctx); err != nil {
		return nil, err
	}
	return &oneRowIterator{row: storage.Row{
		Timestamp: time.Unix(0, 1).UTC(),
		Labels:    storage.LabelSet{"service_name": "waf"},
		Line:      "recorded",
	}}, nil
}

func (s *recordingStore) LabelNames(ctx context.Context, _ storage.TimeRange) ([]string, error) {
	return nil, s.note(ctx)
}

func (s *recordingStore) LabelValues(ctx context.Context, _ string, _ storage.TimeRange) ([]string, error) {
	return nil, s.note(ctx)
}

func (s *recordingStore) Series(ctx context.Context, _ []logql.LabelMatcher, _ storage.TimeRange) ([]storage.LabelSet, error) {
	return nil, s.note(ctx)
}

func (s *recordingStore) Ping(context.Context) error { return nil }

type oneRowIterator struct {
	row  storage.Row
	done bool
}

func (i *oneRowIterator) Next() bool {
	if i.done {
		return false
	}
	i.done = true
	return true
}

func (i *oneRowIterator) Row() storage.Row { return i.row }
func (i *oneRowIterator) Err() error       { return nil }
func (i *oneRowIterator) Close() error     { return nil }

// TestForwardedPathShapes covers every path shape queryapi is reached by.
//
// kube-aggregator forwards the path verbatim, so production sends
// /apis/o11y.miloapis.com/v1alpha1/...; Grafana's Loki datasource sends a bare
// /loki/api/v1/...; and /v1alpha1/... is the form this service assumed before
// it served discovery. All three route to the same handler and are reviewed as
// the same permission. Anything else is not rewritten and not served: a stray
// /projects or /apis prefix is a caller's invention, not tenancy.
func TestForwardedPathShapes(t *testing.T) {
	cases := []struct {
		name string
		path string
		want int // expected status code
	}{
		{"aggregator form", prefix + "/logs/loki/api/v1/query_range", http.StatusOK},
		{"bare loki form", "/loki/api/v1/query_range", http.StatusOK},
		{"versioned form", "/v1alpha1/logs/loki/api/v1/query_range", http.StatusOK},
		// Not our group, and no rewriting will make it ours. These are 404s
		// rather than 403s only because this test's reviewer allows
		// everything: they are outside o11y.miloapis.com, so the guard passes
		// them to Milo like any other path, and nothing serves them.
		{
			"another group is not routed",
			"/apis/telemetry.miloapis.com/v1alpha1/logs/loki/api/v1/query_range", http.StatusNotFound,
		},
		{
			"project prefix not routed",
			"/projects/proj-abc/control-plane/v1alpha1/logs/loki/api/v1/query_range", http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingStore{}
			cfg := queryapi.DefaultConfig()
			srv := serve(t, store, cfg, allowAll())

			req, err := http.NewRequest(http.MethodGet,
				srv.URL+tc.path+`?query={service_name="waf"}&limit=5`, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			// Escaped extras keys, as kube-aggregator sets them. Assigned
			// directly because Header.Set canonicalises.
			req.Header.Set("X-Remote-User", "user@example.com")
			req.Header["X-Remote-Extra-Iam.miloapis.com%2Fparent-type"] = []string{"Project"}
			req.Header["X-Remote-Extra-Iam.miloapis.com%2Fparent-name"] = []string{"proj-abc"}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer func() {
				if err := resp.Body.Close(); err != nil {
					t.Logf("close response body: %v", err)
				}
			}()

			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d (path %q): %s", resp.StatusCode, tc.want, tc.path, body)
			}
			if tc.want != http.StatusOK {
				return
			}

			var payload struct {
				Status string `json:"status"`
				Data   struct {
					Result []struct {
						Values [][2]string `json:"values"`
					} `json:"result"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if payload.Status != "success" || len(payload.Data.Result) != 1 {
				t.Fatalf("payload = %+v, want one stream", payload)
			}
			if got := payload.Data.Result[0].Values[0][1]; got != "recorded" {
				t.Errorf("line = %q, want %q", got, "recorded")
			}
			if got := store.seen(); got != "proj-abc" {
				t.Errorf("backend saw project %q, want %q", got, "proj-abc")
			}
		})
	}
}

// TestGrafanaHealthProbe pins the one metric query this service answers.
// Grafana's Loki datasource CheckHealth sends vector(1)+vector(1); rejecting it
// as a metric query makes the datasource show as failed even though every real
// query works. Anything else metric-shaped must still be a 400.
func TestGrafanaHealthProbe(t *testing.T) {
	cfg := queryapi.DefaultConfig()
	srv := serve(t, fake.New(2), cfg, allowAll())

	get := func(t *testing.T, query string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet,
			srv.URL+prefix+"/logs/loki/api/v1/query?query="+url.QueryEscape(query), nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		forwardIdentity(req)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Logf("close response body: %v", err)
			}
		}()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	// Grafana requires EXACTLY ONE sample here: an empty result fails with
	// "invalid dataframe length, expected 1 got 0" and the datasource is marked
	// failed even though every real query works.
	t.Run("probe answers with exactly one sample", func(t *testing.T) {
		code, body := get(t, "vector(1)+vector(1)")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", code, body)
		}

		var payload struct {
			Status string `json:"status"`
			Data   struct {
				ResultType string `json:"resultType"`
				Result     []struct {
					Metric map[string]string `json:"metric"`
					Value  []any             `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if payload.Status != "success" || payload.Data.ResultType != "vector" {
			t.Fatalf("payload = %+v, want success/vector", payload)
		}
		if len(payload.Data.Result) != 1 {
			t.Fatalf("got %d samples, want exactly 1", len(payload.Data.Result))
		}

		sample := payload.Data.Result[0]
		if len(sample.Value) != 2 {
			t.Fatalf("value = %v, want [timestamp, value]", sample.Value)
		}
		if _, ok := sample.Value[0].(float64); !ok {
			t.Errorf("value[0] = %T, want a numeric timestamp", sample.Value[0])
		}
		// vector(1)+vector(1) evaluates to 2, as a string, per Prometheus.
		if got, ok := sample.Value[1].(string); !ok || got != "2" {
			t.Errorf("value[1] = %v (%T), want the string \"2\"", sample.Value[1], sample.Value[1])
		}
	})

	t.Run("other metric queries are still rejected", func(t *testing.T) {
		for _, q := range []string{
			`rate({service_name="waf"}[5m])`,
			`sum by (service_name) (count_over_time({service_name="waf"}[5m]))`,
			`vector(1)`,
			`vector(1) + vector(1)`, // spacing differs from the probe
		} {
			if code, body := get(t, q); code != http.StatusBadRequest {
				t.Errorf("query %q: status = %d, want 400: %s", q, code, body)
			}
		}
	})
}

// TestCraftedProjectPathIsNotAnIdentity pins the anchoring: a /projects/
// segment appearing later in the path is client-controlled data, and must
// never become the project a query is scoped to.
func TestCraftedProjectPathIsNotAnIdentity(t *testing.T) {
	const crafted = prefix + "/logs/loki/api/v1/label/projects/evil-corp/control-plane/v1alpha1/logs/loki/api/v1/query"

	t.Run("no identity at all", func(t *testing.T) {
		store := &recordingStore{}
		srv := serve(t, store, queryapi.DefaultConfig(), allowAll())

		resp, err := http.Get(srv.URL + crafted + `?query={service_name="waf"}&limit=1`)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Logf("close response body: %v", err)
			}
		}()

		// The path matches no route, so the request info resolver describes it
		// as a non-resource request under our group and the guard denies it.
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
		if got := store.seen(); got != "" {
			t.Errorf("backend saw project %q, want none", got)
		}
	})

	t.Run("identity present, crafted path ignored", func(t *testing.T) {
		store := &recordingStore{}
		srv := serve(t, store, queryapi.DefaultConfig(), allowAll())

		req, err := http.NewRequest(http.MethodGet,
			srv.URL+crafted+`?query={service_name="waf"}&limit=1`, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		forwardIdentity(req)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Logf("close response body: %v", err)
			}
		}()

		// The crafted path must never become the project. Identity comes only
		// from the verified user extras; a 200 is served only scoped to
		// proj-abc, and the backend must never see evil-corp.
		if got := store.seen(); got == "evil-corp" {
			t.Fatalf("backend saw project %q from a crafted path", got)
		}
		if resp.StatusCode == http.StatusOK && store.seen() != "proj-abc" {
			t.Errorf("served 200 for project %q, want proj-abc", store.seen())
		}
	})
}

// TestDiscoveryIsServed is the reason this service runs on the apiserver
// runtime at all.
//
// kube-aggregator's availability controller probes GET
// /apis/{group}/{version} on the backend with X-Remote-User
// system:kube-aggregator in system:masters and requires a 2xx or 3xx; an
// APIService that fails it is marked unavailable and never proxied to, so
// without this document not one query would arrive. system:masters is what
// carries the probe past authorization.
func TestDiscoveryIsServed(t *testing.T) {
	// A reviewer that denies everything, to prove the probe does not depend on
	// one: the availability of the APIService must not turn on Milo's answer.
	srv := serve(t, fake.New(2), queryapi.DefaultConfig(),
		withAuthorizer(&stubAuthorizer{decision: authorizer.DecisionDeny}))

	probe := func(r *http.Request) {
		r.Header.Set("X-Remote-User", "system:kube-aggregator")
		r.Header["X-Remote-Group"] = []string{"system:masters"}
	}

	t.Run("version discovery", func(t *testing.T) {
		code, body := doBody(t, srv, http.MethodGet, prefix, probe)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", code, body)
		}

		var list struct {
			Kind         string `json:"kind"`
			GroupVersion string `json:"groupVersion"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatalf("decode body: %v (body %s)", err, body)
		}
		if list.Kind != "APIResourceList" || list.GroupVersion != "o11y.miloapis.com/v1alpha1" {
			t.Errorf("body = %+v, want an APIResourceList for o11y.miloapis.com/v1alpha1", list)
		}
	})

	t.Run("group discovery", func(t *testing.T) {
		code, body := doBody(t, srv, http.MethodGet, "/apis/o11y.miloapis.com", probe)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", code, body)
		}

		var group struct {
			Kind             string `json:"kind"`
			Name             string `json:"name"`
			PreferredVersion struct {
				GroupVersion string `json:"groupVersion"`
			} `json:"preferredVersion"`
		}
		if err := json.Unmarshal(body, &group); err != nil {
			t.Fatalf("decode body: %v (body %s)", err, body)
		}
		if group.Kind != "APIGroup" || group.Name != "o11y.miloapis.com" ||
			group.PreferredVersion.GroupVersion != "o11y.miloapis.com/v1alpha1" {
			t.Errorf("body = %+v, want the o11y.miloapis.com APIGroup", group)
		}
	})

	// Without the bypass, the same probe is reviewed like anything else.
	t.Run("discovery is authorized for everyone else", func(t *testing.T) {
		code, _ := doBody(t, srv, http.MethodGet, prefix, forwardIdentity)
		if code != http.StatusForbidden {
			t.Errorf("status = %d, want 403 for a caller outside system:masters", code)
		}
	})
}

// TestProbesAreServedWithoutAuthorization pins the other half of requirement
// the kubelet depends on: /healthz, /readyz, /livez and /metrics answer for a
// caller with no credentials at all, because --authorization-always-allow-paths
// covers them. They are non-resource paths, and the path authorizer abstains
// on every resource request, so nothing listed there can open a query route.
func TestProbesAreServedWithoutAuthorization(t *testing.T) {
	srv := serve(t, fake.New(2), queryapi.DefaultConfig(),
		withAuthorizer(&stubAuthorizer{decision: authorizer.DecisionDeny}))

	for _, path := range []string{"/healthz", "/livez", "/readyz", "/metrics"} {
		code, body := doBody(t, srv, http.MethodGet, path, nil)
		if code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200: %s", path, code, body)
		}
	}
}

// TestReadyzReportsStorageUnavailable pins that readiness follows the storage
// backend and liveness does not: an outage that only makes queries fail must
// take the pod out of service, not restart it.
func TestReadyzReportsStorageUnavailable(t *testing.T) {
	srv := serve(t, &unreachableStore{}, queryapi.DefaultConfig(), allowAll())

	code, body := doBody(t, srv, http.MethodGet, "/readyz", nil)
	if code != http.StatusInternalServerError {
		t.Errorf("/readyz status = %d, want 500: %s", code, body)
	}
	if !strings.Contains(string(body), "[-]storage failed") {
		t.Errorf("/readyz body = %s, want the storage check to be the one that failed", body)
	}
	if code, _ := doBody(t, srv, http.MethodGet, "/livez", nil); code != http.StatusOK {
		t.Errorf("/livez status = %d, want 200", code)
	}
	if code, _ := doBody(t, srv, http.MethodGet, "/healthz", nil); code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", code)
	}
}

// unreachableStore fails Ping so the readiness check actually fails.
type unreachableStore struct{ recordingStore }

func (*unreachableStore) Ping(context.Context) error { return errors.New("dial: connection refused") }

var _ storage.LogStore = (*unreachableStore)(nil)
