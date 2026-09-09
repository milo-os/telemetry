// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apiserver/pkg/endpoints/request"

	"go.datum.net/o11y/queryapi/internal/authz"
	"go.datum.net/o11y/queryapi/internal/storage/fake"
)

// TestCanonicalPath pins the anchoring directly. An end-to-end test cannot
// cover all of it: an unrecognised path is rejected by authorization before it
// reaches a route, so a regression here would stay invisible at the HTTP layer.
func TestCanonicalPath(t *testing.T) {
	const prefix = APIPrefix

	cases := []struct {
		name string
		path string
		want string
	}{
		{"already canonical", prefix + "/logs/loki/api/v1/query", prefix + "/logs/loki/api/v1/query"},

		// Grafana omits the datasource URL's subpath, so the bare Loki form
		// must normalise onto the aggregator's own.
		{"bare loki query", "/loki/api/v1/query", prefix + "/logs/loki/api/v1/query"},
		{"bare loki labels", "/loki/api/v1/labels", prefix + "/logs/loki/api/v1/labels"},
		{
			"bare loki label values",
			"/loki/api/v1/label/severity/values",
			prefix + "/logs/loki/api/v1/label/severity/values",
		},

		// The form queryapi assumed before it served discovery.
		{"versioned logs form", "/v1alpha1/logs/loki/api/v1/query", prefix + "/logs/loki/api/v1/query"},
		{"versioned metrics form", "/v1alpha1/metrics/api/v1/query", prefix + "/metrics/api/v1/query"},

		// Anchoring: a bare /loki/api/v1 segment is only a route marker when it
		// leads the path. As a later label-name segment it is client-controlled
		// data and must survive untouched.
		{
			"mid-path bare loki segment is not normalised",
			prefix + "/logs/loki/api/v1/label/loki/api/v1/query/values",
			prefix + "/logs/loki/api/v1/label/loki/api/v1/query/values",
		},
		{
			"mid-path version segment is not normalised",
			prefix + "/logs/loki/api/v1/label/v1alpha1/values",
			prefix + "/logs/loki/api/v1/label/v1alpha1/values",
		},

		// A stray tenancy prefix is left alone rather than half-normalised:
		// Milo's proxy chain consumes /projects/{id}/control-plane before the
		// request arrives, so one appearing here is a caller's invention.
		{
			"leading project prefix is left intact",
			"/projects/p/control-plane/loki/api/v1/query",
			"/projects/p/control-plane/loki/api/v1/query",
		},
		{
			"another group's prefix is left intact",
			"/apis/telemetry.miloapis.com/v1alpha1/logs/loki/api/v1/query",
			"/apis/telemetry.miloapis.com/v1alpha1/logs/loki/api/v1/query",
		},
		{
			"an unknown signal under our version is left intact",
			"/v1alpha1/traces/api/v1/query",
			"/v1alpha1/traces/api/v1/query",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := withCanonicalPaths(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.URL.Path
			}))

			req := httptest.NewRequest(http.MethodGet, tc.path+"?query=x&limit=1", nil)
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got != tc.want {
				t.Errorf("path = %q, want %q", got, tc.want)
			}
			if req.URL.Path != tc.path {
				t.Errorf("original request mutated: %q, want %q", req.URL.Path, tc.path)
			}
		})
	}
}

// TestCanonicalPathPreservesQuery guards the clone: rewriting the path must not
// drop the query string the handlers parse.
func TestCanonicalPathPreservesQuery(t *testing.T) {
	var got string
	h := withCanonicalPaths(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/loki/api/v1/query?query=%7Bservice_name%3D%22waf%22%7D&limit=7", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != `query=%7Bservice_name%3D%22waf%22%7D&limit=7` {
		t.Errorf("RawQuery = %q, want it preserved through the clone", got)
	}
}

// TestRequestInfoResolverIsFailClosed is the proof that authorization cannot be
// slipped: under this service's group, the resolver describes a request as a
// resource request only when the route table matched it. Everything else --
// an unregistered path, the wrong method, a path that would be redirected --
// comes back as a non-resource request, which authz's guard denies and which
// the tenant mux would answer 404 anyway.
func TestRequestInfoResolverIsFailClosed(t *testing.T) {
	router, err := newRouter(slog.New(slog.DiscardHandler), fake.New(2), DefaultConfig())
	if err != nil {
		t.Fatalf("newRouter: %v", err)
	}
	// The fallback must never be reached for a path under the group; a panic
	// says loudly if it is.
	resolver := router.requestInfoResolver(resolverFunc(func(*http.Request) (*request.RequestInfo, error) {
		t.Error("the stock resolver was consulted for a path under our own group")
		return &request.RequestInfo{}, nil
	}))

	cases := []struct {
		name         string
		method       string
		path         string
		wantResource string
		wantVerb     string
	}{
		{"mapped query", http.MethodGet, APIPrefix + "/logs/loki/api/v1/query", "logs", "query"},
		{
			"mapped label values", http.MethodGet,
			APIPrefix + "/logs/loki/api/v1/label/severity/values", "logs", "getLabels",
		},
		{"mapped series", http.MethodGet, APIPrefix + "/logs/loki/api/v1/series", "logs", "getSeries"},
		{"mapped metrics query", http.MethodPost, APIPrefix + "/metrics/api/v1/query", "metrics", "query"},

		// Declared in openapi.yaml, unimplemented, and therefore unmapped: the
		// endpoint most likely to be added without an authorization entry.
		{"unmapped tail", http.MethodGet, APIPrefix + "/logs/loki/api/v1/tail", "", ""},
		{"unmapped signal", http.MethodGet, APIPrefix + "/traces/api/v1/query", "", ""},
		{"wrong method", http.MethodPost, APIPrefix + "/logs/loki/api/v1/query", "", ""},
		{"group root", http.MethodGet, "/apis/" + authz.APIGroup, "", ""},
		{"version root", http.MethodGet, APIPrefix, "", ""},
		{"trailing slash", http.MethodGet, APIPrefix + "/", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := resolver.NewRequestInfo(httptest.NewRequest(tc.method, tc.path, nil))
			if err != nil {
				t.Fatalf("NewRequestInfo: %v", err)
			}
			if tc.wantResource == "" {
				if info.IsResourceRequest {
					t.Fatalf("%s %s resolved to resource %q/%q, want a non-resource request",
						tc.method, tc.path, info.Resource, info.Verb)
				}
				if info.Path != tc.path {
					t.Errorf("path = %q, want %q", info.Path, tc.path)
				}
				return
			}
			if !info.IsResourceRequest {
				t.Fatalf("%s %s resolved to a non-resource request, want %q/%q",
					tc.method, tc.path, tc.wantResource, tc.wantVerb)
			}
			if info.APIGroup != authz.APIGroup || info.APIVersion != authz.APIVersion {
				t.Errorf("group/version = %s/%s, want %s/%s",
					info.APIGroup, info.APIVersion, authz.APIGroup, authz.APIVersion)
			}
			if info.Resource != tc.wantResource || info.Verb != tc.wantVerb {
				t.Errorf("resource/verb = %s.%s, want %s.%s",
					info.Resource, info.Verb, tc.wantResource, tc.wantVerb)
			}
			// A name or a namespace would reach the SubjectAccessReview as
			// caller-controlled data.
			if info.Name != "" || info.Namespace != "" || info.Subresource != "" {
				t.Errorf("attributes carry caller data: name=%q namespace=%q subresource=%q",
					info.Name, info.Namespace, info.Subresource)
			}
		})
	}
}

// TestRequestInfoResolverDelegatesOutsideTheGroup pins the other half: the
// probe and metrics paths --authorization-always-allow-paths is written
// against are resolved by the framework's own resolver, which is what that
// flag matches on.
func TestRequestInfoResolverDelegatesOutsideTheGroup(t *testing.T) {
	router, err := newRouter(slog.New(slog.DiscardHandler), fake.New(2), DefaultConfig())
	if err != nil {
		t.Fatalf("newRouter: %v", err)
	}

	delegated := false
	resolver := router.requestInfoResolver(resolverFunc(func(r *http.Request) (*request.RequestInfo, error) {
		delegated = true
		return &request.RequestInfo{Path: r.URL.Path}, nil
	}))

	for _, path := range []string{"/healthz", "/metrics", "/apis", "/apis/other.miloapis.com/v1"} {
		delegated = false
		if _, err := resolver.NewRequestInfo(httptest.NewRequest(http.MethodGet, path, nil)); err != nil {
			t.Fatalf("NewRequestInfo(%s): %v", path, err)
		}
		if !delegated {
			t.Errorf("%s was not delegated to the stock resolver", path)
		}
	}
}

// resolverFunc adapts a function to request.RequestInfoResolver.
type resolverFunc func(*http.Request) (*request.RequestInfo, error)

func (f resolverFunc) NewRequestInfo(r *http.Request) (*request.RequestInfo, error) { return f(r) }
