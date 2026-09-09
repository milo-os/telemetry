// SPDX-License-Identifier: AGPL-3.0-only

package miloauth_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.datum.net/o11y/queryapi/internal/miloauth"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name   string
		user   user.Info
		wantID string
		wantOK bool
	}{
		{name: "no user", user: nil},
		{
			name: "project parent",
			user: &user.DefaultInfo{Extra: map[string][]string{
				"iam.miloapis.com/parent-type": {"Project"},
				"iam.miloapis.com/parent-name": {"proj-abc"},
			}},
			wantID: "proj-abc", wantOK: true,
		},
		{
			// Milo sets the same extras for organization parents, so the type
			// check is what keeps an organization id from being read as a
			// project id.
			name: "organization parent",
			user: &user.DefaultInfo{Extra: map[string][]string{
				"iam.miloapis.com/parent-type": {"Organization"},
				"iam.miloapis.com/parent-name": {"org-abc"},
			}},
		},
		{
			name: "project parent with an unusable name",
			user: &user.DefaultInfo{Extra: map[string][]string{
				"iam.miloapis.com/parent-type": {"Project"},
				"iam.miloapis.com/parent-name": {"../evil-corp"},
			}},
		},
		{
			name: "project parent with a name that is not an identifier",
			user: &user.DefaultInfo{Extra: map[string][]string{
				"iam.miloapis.com/parent-type": {"Project"},
				"iam.miloapis.com/parent-name": {"Not A Project!"},
			}},
		},
		{
			// The delegating authenticator admits system:anonymous, so an
			// unauthenticated caller reaches Resolve. It carries no parent
			// extras, so it scopes to nothing.
			name: "anonymous caller",
			user: &user.DefaultInfo{Name: user.Anonymous, Groups: []string{user.AllUnauthenticated}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := miloauth.Resolve(tc.user)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (id=%q)", ok, tc.wantOK, id)
			}
			if id != tc.wantID {
				t.Errorf("id = %q, want %q", id, tc.wantID)
			}
		})
	}
}

// TestMiddlewarePassesTheProjectToTheStore pins the one thing the filter does:
// the project the caller was authorized for is the project the query is bound
// to, because both come from the same Resolve on the same verified identity.
func TestMiddlewarePassesTheProjectToTheStore(t *testing.T) {
	var got string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := miloauth.ProjectID(r.Context())
		if !ok {
			t.Error("no project on context inside the handler")
		}
		got = id
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/apis/o11y.miloapis.com/v1alpha1/logs/loki/api/v1/query", nil)
	r = r.WithContext(request.WithUser(r.Context(), &user.DefaultInfo{
		Name: "user@example.com",
		Extra: map[string][]string{
			"iam.miloapis.com/parent-type": {"Project"},
			"iam.miloapis.com/parent-name": {"proj-abc"},
		},
	}))

	rec := httptest.NewRecorder()
	miloauth.Middleware(slog.New(slog.DiscardHandler), next).ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got != "proj-abc" {
		t.Errorf("project = %q, want proj-abc", got)
	}
}

// TestMiddlewareFailsClosed pins the guard behind the guard. Authorization has
// already refused an unscoped caller by the time anything here runs, so these
// are unreachable in the assembled server -- which is exactly why they are
// worth pinning: a future reordering must fail closed rather than serve one
// project's telemetry unscoped.
func TestMiddlewareFailsClosed(t *testing.T) {
	cases := map[string]user.Info{
		"no caller at all":  nil,
		"no project parent": &user.DefaultInfo{Name: "user@example.com"},
		"an organization parent": &user.DefaultInfo{Name: "user@example.com", Extra: map[string][]string{
			"iam.miloapis.com/parent-type": {"Organization"},
			"iam.miloapis.com/parent-name": {"org-abc"},
		}},
	}

	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

			r := httptest.NewRequest(http.MethodGet, "/apis/o11y.miloapis.com/v1alpha1/logs/loki/api/v1/query", nil)
			if u != nil {
				r = r.WithContext(request.WithUser(r.Context(), u))
			}
			// A header a client set for itself has never named the project and
			// must not start now.
			r.Header["X-Project-Id"] = []string{"evil-corp"}

			rec := httptest.NewRecorder()
			miloauth.Middleware(slog.New(slog.DiscardHandler), next).ServeHTTP(rec, r)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if called {
				t.Error("the handler ran for a request that resolved to no project")
			}
		})
	}
}
