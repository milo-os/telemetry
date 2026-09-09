// SPDX-License-Identifier: AGPL-3.0-only

package queryapi_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/request/anonymous"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/authentication/request/union"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	"k8s.io/apiserver/pkg/authorization/path"
	authzunion "k8s.io/apiserver/pkg/authorization/union"
	"k8s.io/client-go/rest"

	"go.datum.net/o11y/queryapi"
	"go.datum.net/o11y/queryapi/internal/authz"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// testAuthenticator reads the X-Remote-* headers Milo's aggregator sets, the
// way the delegating authenticator does once it has verified the front proxy's
// client certificate -- which a test server speaking plain HTTP cannot
// present. Header parsing, including the escaped extras keys, is the real
// implementation; only the certificate check is stood in for, and options.go
// is the only place that check is configured.
func testAuthenticator() authenticator.Request {
	authn, err := headerrequest.New(
		[]string{"X-Remote-User"},
		[]string{"X-Remote-Uid"},
		[]string{"X-Remote-Group"},
		[]string{"X-Remote-Extra-"},
	)
	if err != nil {
		// Only reachable if the header names above were malformed.
		panic(err)
	}
	// The delegating authenticator ends its union with the anonymous
	// authenticator, which is why an unauthenticated caller here is
	// system:anonymous and gets a 403 rather than a 401. Probes depend on it:
	// the kubelet presents no credentials.
	return union.NewFailOnError(authn, anonymous.NewAuthenticator(nil))
}

// stubAuthorizer stands in for Milo's SubjectAccessReview endpoint: it answers
// with a fixed decision and records what it was asked.
type stubAuthorizer struct {
	decision authorizer.Decision
	err      error

	mu    sync.Mutex
	calls []authorizer.Attributes
}

func (s *stubAuthorizer) Authorize(
	_ context.Context,
	attrs authorizer.Attributes,
) (authorizer.Decision, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, attrs)
	if s.err != nil {
		return authorizer.DecisionDeny, "", s.err
	}
	return s.decision, "stub", nil
}

// reviews returns the attributes the stub was asked about for this service's
// own resources. Requests for anything else -- probes, discovery -- are not
// telemetry reviews and are not counted.
func (s *stubAuthorizer) reviews() []authorizer.Attributes {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []authorizer.Attributes
	for _, a := range s.calls {
		if a.IsResourceRequest() && a.GetAPIGroup() == authz.APIGroup {
			out = append(out, a)
		}
	}
	return out
}

// last returns the most recent telemetry review, failing the test if there was
// none.
func (s *stubAuthorizer) last(t *testing.T) authorizer.Attributes {
	t.Helper()
	reviews := s.reviews()
	if len(reviews) == 0 {
		t.Fatal("the authorizer saw no telemetry review")
	}
	return reviews[len(reviews)-1]
}

// failingAuthenticator stands in for an authenticator that cannot reach what
// it needs, such as an unavailable TokenReview endpoint.
type failingAuthenticator struct{}

func (failingAuthenticator) AuthenticateRequest(*http.Request) (*authenticator.Response, bool, error) {
	return nil, false, errors.New("token review unavailable")
}

// deps are the two collaborators a real process gets from the cluster and a
// test has to supply.
type deps struct {
	authn authenticator.Request
	authz authorizer.Authorizer
}

// allowAll builds dependencies that authenticate the forwarded headers and
// authorize everything, so a test about something other than authorization
// does not have to describe it.
func allowAll() deps {
	return withAuthorizer(&stubAuthorizer{decision: authorizer.DecisionAllow})
}

// withAuthorizer builds dependencies with a specific delegate, wrapped in the
// same chain a real process runs: privileged groups, then the unauthorized
// probe paths, then the SubjectAccessReview. Only the last link is a stub.
func withAuthorizer(delegate authorizer.Authorizer) deps {
	alwaysAllowPaths, err := path.NewAuthorizer([]string{"/healthz", "/readyz", "/livez", "/metrics"})
	if err != nil {
		panic(err)
	}
	chain := []authorizer.Authorizer{
		authorizerfactory.NewPrivilegedGroups(user.SystemPrivilegedGroup),
		alwaysAllowPaths,
	}
	if delegate != nil {
		chain = append(chain, delegate)
	}
	return deps{authn: testAuthenticator(), authz: authzunion.New(chain...)}
}

// serve builds the real server -- the generic apiserver, its handler chain,
// its discovery documents and the tenant routes -- and puts it behind an HTTP
// test server.
//
// It speaks plain HTTP, which the production listener never does. That costs
// exactly one thing, the front proxy's client certificate check, which
// testAuthenticator stands in for; every other filter, the route table, the
// request info resolver and the authorization chain are the real ones.
func serve(t *testing.T, store storage.LogStore, cfg queryapi.Config, d deps) *httptest.Server {
	t.Helper()

	generic := queryapi.NewGenericConfig()
	// A real process gets both of these from the secure listener it opened.
	// There is none here, and the framework refuses to guess. The loopback
	// config carries no bearer token, so it grants nothing.
	generic.ExternalAddress = "queryapi.test:443"
	generic.LoopbackClientConfig = &rest.Config{Host: "https://" + generic.ExternalAddress}
	generic.Authentication.Authenticator = d.authn
	generic.Authorization.Authorizer = d.authz

	serverConfig := &queryapi.ServerConfig{
		Generic: generic,
		Extra: queryapi.ExtraConfig{
			Logger: slog.New(slog.DiscardHandler),
			Store:  store,
			Query:  cfg,
		},
	}
	completed, err := serverConfig.Complete()
	if err != nil {
		t.Fatalf("complete the server configuration: %v", err)
	}
	server, err := completed.New()
	if err != nil {
		t.Fatalf("build the server: %v", err)
	}
	// PrepareRun installs /healthz, /livez and /readyz; the post-start hooks
	// are what make them pass. A real process runs both from Run.
	server.GenericAPIServer.PrepareRun()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server.GenericAPIServer.RunPostStartHooks(ctx)
	waitForReady(t, server)

	srv := httptest.NewServer(server.GenericAPIServer.Handler)
	t.Cleanup(srv.Close)
	return srv
}

// waitForReady blocks until the post-start hooks have reported, so a probe
// assertion is not racing them.
func waitForReady(t *testing.T, server *queryapi.Server) {
	t.Helper()
	rec := httptest.NewRecorder()
	for range 100 {
		rec = httptest.NewRecorder()
		// Director, not the full chain: this test server is sometimes built
		// with an authenticator that fails on purpose, and readiness is not
		// what that test is about.
		server.GenericAPIServer.Handler.Director.ServeHTTP(rec,
			httptest.NewRequest(http.MethodGet, "/livez", nil))
		if rec.Code == http.StatusOK {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("post-start hooks never reported healthy: %s", rec.Body.String())
}
