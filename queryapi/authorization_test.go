// SPDX-License-Identifier: AGPL-3.0-only

package queryapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"k8s.io/apiserver/pkg/authorization/authorizer"

	"go.datum.net/o11y/queryapi"
	"go.datum.net/o11y/queryapi/internal/authz"
	"go.datum.net/o11y/queryapi/internal/storage/fake"
)

// prefix is the path kube-aggregator forwards under: handler_proxy.go rewrites
// the host and leaves the path alone, so this is what production actually
// sends.
const prefix = queryapi.APIPrefix

// forwardIdentity sets the headers Milo's aggregator forwards for a caller in
// proj-abc. The extras keys are escaped and assigned directly because
// Header.Set canonicalises them.
func forwardIdentity(req *http.Request) {
	req.Header.Set("X-Remote-User", "user@example.com")
	req.Header.Set("X-Remote-Uid", "user-uid-123")
	req.Header["X-Remote-Group"] = []string{"engineering", "oncall"}
	req.Header["X-Remote-Extra-Iam.miloapis.com%2Fparent-type"] = []string{"Project"}
	req.Header["X-Remote-Extra-Iam.miloapis.com%2Fparent-name"] = []string{"proj-abc"}
}

// authorizedRoutes is every route the router registers, with the status it
// answers once the caller is authorized and the permission it is reviewed for.
// The metrics handlers are stubs, hence the 501s -- they are gated all the same.
var authorizedRoutes = []struct {
	name    string
	method  string
	path    string
	allowed int
	perm    authz.Permission
}{
	{
		"loki query", http.MethodGet, `/logs/loki/api/v1/query?query={service_name="waf"}`,
		http.StatusOK, authz.Permission{Resource: "logs", Verb: "query"},
	},
	{
		"loki query_range", http.MethodGet, `/logs/loki/api/v1/query_range?query={service_name="waf"}`,
		http.StatusOK, authz.Permission{Resource: "logs", Verb: "query"},
	},
	{
		"loki labels", http.MethodGet, "/logs/loki/api/v1/labels",
		http.StatusOK, authz.Permission{Resource: "logs", Verb: "getLabels"},
	},
	{
		"loki label values", http.MethodGet, "/logs/loki/api/v1/label/severity/values",
		http.StatusOK, authz.Permission{Resource: "logs", Verb: "getLabels"},
	},
	{
		"loki series", http.MethodGet, `/logs/loki/api/v1/series?match[]={service_name="waf"}`,
		http.StatusOK, authz.Permission{Resource: "logs", Verb: "getSeries"},
	},
	{
		"prom query", http.MethodPost, "/metrics/api/v1/query",
		http.StatusNotImplemented, authz.Permission{Resource: "metrics", Verb: "query"},
	},
	{
		"prom query_range", http.MethodPost, "/metrics/api/v1/query_range",
		http.StatusNotImplemented, authz.Permission{Resource: "metrics", Verb: "query"},
	},
	{
		"prom labels", http.MethodGet, "/metrics/api/v1/labels",
		http.StatusNotImplemented, authz.Permission{Resource: "metrics", Verb: "getLabels"},
	},
	{
		"prom label values", http.MethodGet, "/metrics/api/v1/label/service/values",
		http.StatusNotImplemented, authz.Permission{Resource: "metrics", Verb: "getLabels"},
	},
	{
		"prom series", http.MethodGet, "/metrics/api/v1/series",
		http.StatusNotImplemented, authz.Permission{Resource: "metrics", Verb: "getSeries"},
	},
}

// do sends an identified request to srv and returns its status.
func do(t *testing.T, srv *httptest.Server, method, path string) int {
	t.Helper()
	code, _ := doBody(t, srv, method, path, forwardIdentity)
	return code
}

// doBody sends a request shaped by identity and returns its status and body.
func doBody(t *testing.T, srv *httptest.Server, method, path string, identity func(*http.Request)) (int, []byte) {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if identity != nil {
		identity(req)
	}

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
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, body
}

// TestEveryRouteIsAuthorized drives each route with an allowing and a denying
// authorizer, and asserts the permission each was reviewed for. Because the
// request info resolver describes an unmapped path as a non-resource request
// that the guard denies, the allow half also proves the table covers every
// registered route.
func TestEveryRouteIsAuthorized(t *testing.T) {
	for _, tc := range authorizedRoutes {
		t.Run(tc.name, func(t *testing.T) {
			allow := &stubAuthorizer{decision: authorizer.DecisionAllow}
			allowed := serve(t, fake.New(2), queryapi.DefaultConfig(), withAuthorizer(allow))

			if got := do(t, allowed, tc.method, prefix+tc.path); got != tc.allowed {
				t.Errorf("allowed: status = %d, want %d", got, tc.allowed)
			}
			reviews := allow.reviews()
			if len(reviews) != 1 {
				t.Fatalf("authorizer saw %d telemetry reviews, want 1", len(reviews))
			}
			attrs := reviews[0]
			if attrs.GetResource() != tc.perm.Resource || attrs.GetVerb() != tc.perm.Verb {
				t.Errorf("reviewed %s/%s.%s, want %s/%s.%s",
					attrs.GetAPIGroup(), attrs.GetResource(), attrs.GetVerb(),
					authz.APIGroup, tc.perm.Resource, tc.perm.Verb)
			}

			denied := serve(t, fake.New(2), queryapi.DefaultConfig(),
				withAuthorizer(&stubAuthorizer{decision: authorizer.DecisionDeny}))
			if got := do(t, denied, tc.method, prefix+tc.path); got != http.StatusForbidden {
				t.Errorf("denied: status = %d, want 403", got)
			}
		})
	}
}

// TestAuthorizerFailureIsNotServed pins the fail-closed direction: an
// unreachable or misbehaving SubjectAccessReview endpoint must not serve
// telemetry, and no query must reach the backend. The framework answers a
// review that errored with a 500 rather than a 403 -- an outage is not a
// verdict -- and either way nothing is served.
func TestAuthorizerFailureIsNotServed(t *testing.T) {
	cases := map[string]struct {
		delegate authorizer.Authorizer
		want     int
	}{
		"authorizer errors": {
			delegate: &stubAuthorizer{err: errors.New("subjectaccessreview endpoint unreachable")},
			want:     http.StatusInternalServerError,
		},
		"no opinion": {
			delegate: &stubAuthorizer{decision: authorizer.DecisionNoOpinion},
			want:     http.StatusForbidden,
		},
		// No delegate at all: the chain can still allow the probe paths, but
		// nothing can ever allow a query.
		"no reviewer configured": {delegate: nil, want: http.StatusForbidden},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &recordingStore{}
			srv := serve(t, store, queryapi.DefaultConfig(), withAuthorizer(tc.delegate))

			got := do(t, srv, http.MethodGet, prefix+`/logs/loki/api/v1/query?query={service_name="waf"}`)
			if got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
			if seen := store.seen(); seen != "" {
				t.Errorf("backend saw project %q, want none", seen)
			}
		})
	}
}

// TestNilAuthorizerIsRefused pins that the server cannot be assembled without
// one. The framework's WithAuthorization filter becomes a pass-through when
// the authorizer is nil, which is the one outcome this service must not have.
func TestNilAuthorizerIsRefused(t *testing.T) {
	generic := queryapi.NewGenericConfig()
	generic.ExternalAddress = "queryapi.test:443"
	generic.Authentication.Authenticator = testAuthenticator()
	generic.Authorization.Authorizer = nil

	_, err := (&queryapi.ServerConfig{
		Generic: generic,
		Extra: queryapi.ExtraConfig{
			Logger: discardLogger(),
			Store:  fake.New(2),
			Query:  queryapi.DefaultConfig(),
		},
	}).Complete()
	if err == nil {
		t.Fatal("Complete() = nil error, want a refusal to build a server with no authorizer")
	}
}

// TestReviewCarriesTheWholeCaller pins what the SubjectAccessReview is built
// from. Authorization is delegated to Milo, which is where tenancy is
// evaluated, and it evaluates it from the caller's extras: an authorizer that
// only saw a username could not tell projects apart, and one that only saw the
// route could not evaluate a policy bound to a group.
func TestReviewCarriesTheWholeCaller(t *testing.T) {
	az := &stubAuthorizer{decision: authorizer.DecisionAllow}
	srv := serve(t, fake.New(2), queryapi.DefaultConfig(), withAuthorizer(az))

	if got := do(t, srv, http.MethodGet, prefix+"/logs/loki/api/v1/label/severity/values"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	attrs := az.last(t)
	if got := attrs.GetUser().GetName(); got != "user@example.com" {
		t.Errorf("user = %q, want user@example.com", got)
	}
	if got := attrs.GetUser().GetGroups(); len(got) != 2 || got[0] != "engineering" || got[1] != "oncall" {
		t.Errorf("groups = %v, want [engineering oncall]", got)
	}
	// Milo's IAM resolves the caller to iam.miloapis.com/InternalUser:<uid> and
	// authorizes that, not the username. A review carrying an empty UID matches
	// no grant and denies every query, so the UID has to survive the proxy hop
	// into the review -- which is what --requestheader-uid-headers configures.
	if got := attrs.GetUser().GetUID(); got != "user-uid-123" {
		t.Errorf("uid = %q, want user-uid-123", got)
	}
	extra := attrs.GetUser().GetExtra()
	// The whole of tenancy: Milo reads the project from here, so it has to
	// survive authentication into the review.
	if got := extra["iam.miloapis.com/parent-type"]; len(got) != 1 || got[0] != "Project" {
		t.Errorf("parent-type extra = %v, want [Project]", got)
	}
	if got := extra["iam.miloapis.com/parent-name"]; len(got) != 1 || got[0] != "proj-abc" {
		t.Errorf("parent-name extra = %v, want [proj-abc]", got)
	}

	// o11y.miloapis.com/logs.getLabels: the metadata action, not the one that
	// returns log lines.
	if attrs.GetAPIGroup() != authz.APIGroup || attrs.GetResource() != "logs" || attrs.GetVerb() != "getLabels" {
		t.Errorf("attributes = %s/%s.%s, want %s/logs.getLabels",
			attrs.GetAPIGroup(), attrs.GetResource(), attrs.GetVerb(), authz.APIGroup)
	}
	// Nothing the caller chose may reach the review.
	if attrs.GetName() != "" || attrs.GetNamespace() != "" || attrs.GetSubresource() != "" {
		t.Errorf("review carries caller data: name=%q namespace=%q subresource=%q",
			attrs.GetName(), attrs.GetNamespace(), attrs.GetSubresource())
	}
}

// TestUnmappedPathIsDeniedWithoutAReview covers the paths a route table can
// grow past: an endpoint declared in openapi.yaml but never registered, a
// signal that does not exist, a crafted prefix. None of them may be reviewed
// as a non-resource URL that Milo might grant broadly, and none may be served.
func TestUnmappedPathIsDeniedWithoutAReview(t *testing.T) {
	paths := map[string]string{
		// Declared in openapi.yaml, unimplemented.
		"tail":                   prefix + "/logs/loki/api/v1/tail",
		"unknown signal":         prefix + "/traces/api/v1/query",
		"crafted tenancy prefix": prefix + "/logs/loki/api/v1/label/projects/evil-corp/control-plane/values",
		"version root slash":     prefix + "/",
	}

	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			az := &stubAuthorizer{decision: authorizer.DecisionAllow}
			store := &recordingStore{}
			srv := serve(t, store, queryapi.DefaultConfig(), withAuthorizer(az))

			if got := do(t, srv, http.MethodGet, path); got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
			if reviews := az.reviews(); len(reviews) != 0 {
				t.Errorf("authorizer saw %d telemetry reviews, want none", len(reviews))
			}
			if seen := store.seen(); seen != "" {
				t.Errorf("backend saw project %q, want none", seen)
			}
		})
	}
}

// TestCraftedProjectPathIsNotReviewed pins the anchoring: a /projects/ segment
// in the path is caller-controlled data. It is a label name and nothing else,
// and it never becomes the tenancy a review is evaluated against.
func TestCraftedProjectPathIsNotReviewed(t *testing.T) {
	az := &stubAuthorizer{decision: authorizer.DecisionAllow}
	srv := serve(t, fake.New(2), queryapi.DefaultConfig(), withAuthorizer(az))

	if got := do(t, srv, http.MethodGet, prefix+"/logs/loki/api/v1/label/projects/values"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	attrs := az.last(t)
	if got := attrs.GetUser().GetExtra()["iam.miloapis.com/parent-name"]; len(got) != 1 || got[0] != "proj-abc" {
		t.Errorf("review carried parent-name %v, want [proj-abc] from the verified identity", got)
	}
	if attrs.GetName() != "" {
		t.Errorf("review carried name %q from the path", attrs.GetName())
	}
}

// TestForbiddenResponseIsTheFrameworkStatus records the one caller-visible
// change of moving onto the apiserver runtime: a denial is a Kubernetes
// Status, written by the framework's own WithAuthorization filter, rather than
// the Loki error envelope the handlers use for their own errors. openapi.yaml
// says so, and this checks that it still does.
func TestForbiddenResponseIsTheFrameworkStatus(t *testing.T) {
	router := loadSpec(t)
	srv := serve(t, fake.New(2), queryapi.DefaultConfig(),
		withAuthorizer(&stubAuthorizer{decision: authorizer.DecisionDeny}))

	const path = "/logs/loki/api/v1/labels"
	code, body := doBody(t, srv, http.MethodGet, prefix+path, forwardIdentity)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", code, body)
	}

	var status struct {
		Kind   string `json:"kind"`
		Status string `json:"status"`
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decode body: %v (body %s)", err, body)
	}
	if status.Kind != "Status" || status.Status != "Failure" || status.Code != http.StatusForbidden {
		t.Errorf("body = %+v, want a metav1.Status Failure with code 403", status)
	}

	specReq, err := http.NewRequest(http.MethodGet, specBase+path, nil)
	if err != nil {
		t.Fatalf("build spec request: %v", err)
	}
	route, pathParams, err := router.FindRoute(specReq)
	if err != nil {
		t.Fatalf("find route for %s: %v", path, err)
	}
	if err := openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    specReq,
			PathParams: pathParams,
			Route:      route,
		},
		Status: code,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(bytes.NewReader(body)),
	}); err != nil {
		t.Fatalf("403 response did not conform to openapi.yaml: %v\nbody: %s", err, body)
	}
}

// TestAuthenticationFailureIsUnauthorized pins that a caller who cannot be
// identified is a 401 and never reaches a review: an unreachable TokenReview
// endpoint must not become an unattributed request that Milo might allow.
func TestAuthenticationFailureIsUnauthorized(t *testing.T) {
	az := &stubAuthorizer{decision: authorizer.DecisionAllow}
	d := withAuthorizer(az)
	d.authn = failingAuthenticator{}

	store := &recordingStore{}
	srv := serve(t, store, queryapi.DefaultConfig(), d)

	got := do(t, srv, http.MethodGet, prefix+`/logs/loki/api/v1/query?query={service_name="waf"}`)
	if got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
	if reviews := az.reviews(); len(reviews) != 0 {
		t.Errorf("authorizer saw %d reviews, want none", len(reviews))
	}
	if seen := store.seen(); seen != "" {
		t.Errorf("backend saw project %q, want none", seen)
	}
}

// TestNoHeaderNamesTheProject pins the removed mode. queryapi once accepted
// X-Project-Id under a flag; nothing a client sets for itself may scope a
// request now. Tenancy rides only in the iam.miloapis.com/parent-* extras of a
// verified identity, so a caller that supplies its own header is simply
// unscoped -- Milo is asked about a caller in no project, and the review the
// stub records must carry no project either.
func TestNoHeaderNamesTheProject(t *testing.T) {
	// Every header that has ever named a project here, plus the shapes a
	// caller might guess at.
	headers := map[string]string{
		"X-Project-Id":       "evil-corp",
		"X-Project":          "evil-corp",
		"X-Scope-OrgID":      "evil-corp", // Loki's own tenant header
		"X-Milo-Project":     "evil-corp",
		"X-Remote-Extra-Foo": "evil-corp",
	}

	for header, value := range headers {
		t.Run(header, func(t *testing.T) {
			// An allow-everything reviewer, so that anything reaching the
			// backend would be visible rather than hidden behind a 403.
			az := &stubAuthorizer{decision: authorizer.DecisionAllow}
			store := &recordingStore{}
			srv := serve(t, store, queryapi.DefaultConfig(), withAuthorizer(az))

			code, _ := doBody(t, srv, http.MethodGet,
				prefix+`/logs/loki/api/v1/query?query={service_name="waf"}`,
				func(r *http.Request) { r.Header[header] = []string{value} })
			if code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", code)
			}
			for _, attrs := range az.reviews() {
				if got := attrs.GetUser().GetExtra()["iam.miloapis.com/parent-name"]; len(got) != 0 {
					t.Errorf("review carried parent-name %v for a caller that named its own project", got)
				}
			}
			if seen := store.seen(); seen != "" {
				t.Errorf("backend saw project %q, want none", seen)
			}
		})
	}
}

// TestUnscopedCallerIsNotServed pins that authenticating is not enough: a
// caller with no project parent reaches no telemetry. Milo decides that, from
// extras that say the caller is in no project -- so the stub, which allows
// everything, is deliberately not the thing keeping this closed. The
// miloauth filter behind the routes is.
func TestUnscopedCallerIsNotServed(t *testing.T) {
	az := &stubAuthorizer{decision: authorizer.DecisionAllow}
	store := &recordingStore{}
	srv := serve(t, store, queryapi.DefaultConfig(), withAuthorizer(az))

	code, _ := doBody(t, srv, http.MethodGet,
		prefix+`/logs/loki/api/v1/query?query={service_name="waf"}`,
		func(r *http.Request) {
			// Authenticated, but not as a caller acting inside a project.
			r.Header.Set("X-Remote-User", "user@example.com")
			r.Header["X-Remote-Extra-Iam.miloapis.com%2Fparent-type"] = []string{"Organization"}
			r.Header["X-Remote-Extra-Iam.miloapis.com%2Fparent-name"] = []string{"org-abc"}
		})
	if code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", code)
	}
	if seen := store.seen(); seen != "" {
		t.Errorf("backend saw project %q, want none", seen)
	}
}

// TestErrorResponsesConformToSpec checks the envelope the handlers themselves
// write, the way TestHandlersConformToSpec checks the success ones.
func TestErrorResponsesConformToSpec(t *testing.T) {
	router := loadSpec(t)
	srv := serve(t, fake.New(2), queryapi.DefaultConfig(), allowAll())

	const path = "/logs/loki/api/v1/series"
	// match[] is required, so this is a handler-written 400.
	code, body := doBody(t, srv, http.MethodGet, prefix+path, forwardIdentity)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", code, body)
	}

	specReq, err := http.NewRequest(http.MethodGet, specBase+path, nil)
	if err != nil {
		t.Fatalf("build spec request: %v", err)
	}
	route, pathParams, err := router.FindRoute(specReq)
	if err != nil {
		t.Fatalf("find route for %s: %v", path, err)
	}

	if err := openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    specReq,
			PathParams: pathParams,
			Route:      route,
		},
		Status: code,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(bytes.NewReader(body)),
	}); err != nil {
		t.Fatalf("400 response did not conform to openapi.yaml: %v\nbody: %s", err, body)
	}
}
