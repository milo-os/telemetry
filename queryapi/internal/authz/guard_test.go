// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
)

// recorder stands in for the framework's delegating authorizer.
type recorder struct {
	decision authorizer.Decision
	err      error
	calls    int
}

func (r *recorder) Authorize(context.Context, authorizer.Attributes) (authorizer.Decision, string, error) {
	r.calls++
	return r.decision, "delegated", r.err
}

func caller() user.Info {
	return &user.DefaultInfo{Name: "user@example.com", Extra: map[string][]string{
		"iam.miloapis.com/parent-type": {"Project"},
		"iam.miloapis.com/parent-name": {"proj-abc"},
	}}
}

// TestGuardOnlyReviewsTheVocabulary is the fail-closed proof at the authorizer
// level: attributes naming this service's group are forwarded to Milo only
// when they name a permission it actually has, and non-resource paths under
// the group only when they are one of the two discovery documents.
func TestGuardOnlyReviewsTheVocabulary(t *testing.T) {
	cases := map[string]struct {
		attrs        authorizer.Attributes
		wantDelegate bool
	}{
		"a known permission": {
			attrs: authorizer.AttributesRecord{
				User: caller(), ResourceRequest: true,
				APIGroup: APIGroup, APIVersion: APIVersion, Resource: "logs", Verb: "getLabels",
			},
			wantDelegate: true,
		},
		"an unknown verb in our group": {
			attrs: authorizer.AttributesRecord{
				User: caller(), ResourceRequest: true,
				APIGroup: APIGroup, APIVersion: APIVersion, Resource: "logs", Verb: "get",
			},
		},
		"an unknown resource in our group": {
			attrs: authorizer.AttributesRecord{
				User: caller(), ResourceRequest: true,
				APIGroup: APIGroup, APIVersion: APIVersion, Resource: "traces", Verb: "query",
			},
		},
		"another version of our group": {
			attrs: authorizer.AttributesRecord{
				User: caller(), ResourceRequest: true,
				APIGroup: APIGroup, APIVersion: "v1beta1", Resource: "logs", Verb: "query",
			},
		},
		// A name or subresource means the attributes did not come from the
		// route table, which is the only thing allowed to describe what this
		// service serves.
		"a permission carrying a name": {
			attrs: authorizer.AttributesRecord{
				User: caller(), ResourceRequest: true, Name: "evil-corp",
				APIGroup: APIGroup, APIVersion: APIVersion, Resource: "logs", Verb: "query",
			},
		},
		"a permission carrying a subresource": {
			attrs: authorizer.AttributesRecord{
				User: caller(), ResourceRequest: true, Subresource: "status",
				APIGroup: APIGroup, APIVersion: APIVersion, Resource: "logs", Verb: "query",
			},
		},
		// The unmapped-path case: an endpoint openapi.yaml declares but no
		// route serves must not be reviewed as a non-resource URL that Milo
		// might grant broadly.
		"an unmapped path under our group": {
			attrs: authorizer.AttributesRecord{
				User: caller(), Verb: "get",
				Path: GroupPrefix + "/" + APIVersion + "/logs/loki/api/v1/tail",
			},
		},
		"the version discovery document": {
			attrs:        authorizer.AttributesRecord{User: caller(), Verb: "get", Path: GroupPrefix + "/" + APIVersion},
			wantDelegate: true,
		},
		"the group discovery document": {
			attrs:        authorizer.AttributesRecord{User: caller(), Verb: "get", Path: GroupPrefix},
			wantDelegate: true,
		},
		// Everything outside the group is the framework's business: probes,
		// metrics, the root discovery document.
		"a probe path": {
			attrs:        authorizer.AttributesRecord{User: caller(), Verb: "get", Path: "/healthz"},
			wantDelegate: true,
		},
		"another group's resource": {
			attrs: authorizer.AttributesRecord{
				User: caller(), ResourceRequest: true,
				APIGroup: "telemetry.miloapis.com", APIVersion: "v1alpha1", Resource: "exportpolicies", Verb: "get",
			},
			wantDelegate: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			delegate := &recorder{decision: authorizer.DecisionAllow}
			guard, err := Guard(delegate)
			if err != nil {
				t.Fatalf("Guard: %v", err)
			}

			decision, _, err := guard.Authorize(context.Background(), tc.attrs)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if tc.wantDelegate {
				if delegate.calls != 1 {
					t.Errorf("delegate saw %d reviews, want 1", delegate.calls)
				}
				if decision != authorizer.DecisionAllow {
					t.Errorf("decision = %v, want the delegate's allow", decision)
				}
				return
			}
			if delegate.calls != 0 {
				t.Errorf("delegate saw %d reviews, want none", delegate.calls)
			}
			if decision != authorizer.DecisionDeny {
				t.Errorf("decision = %v, want deny", decision)
			}
		})
	}
}

// TestGuardPassesFailuresThrough pins that the guard adds no verdict of its
// own to a review that happened: a delegate's error stays an error, so the
// framework answers 500 rather than serving.
func TestGuardPassesFailuresThrough(t *testing.T) {
	want := errors.New("subjectaccessreview endpoint unreachable")
	guard, err := Guard(&recorder{decision: authorizer.DecisionNoOpinion, err: want})
	if err != nil {
		t.Fatalf("Guard: %v", err)
	}

	decision, _, err := guard.Authorize(context.Background(), authorizer.AttributesRecord{
		User: caller(), ResourceRequest: true,
		APIGroup: APIGroup, APIVersion: APIVersion, Resource: "logs", Verb: "query",
	})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if decision == authorizer.DecisionAllow {
		t.Error("decision = allow for a review that failed")
	}
}

// TestGuardRefusesANilDelegate pins that there is no way to assemble a server
// whose authorization filter is a pass-through.
func TestGuardRefusesANilDelegate(t *testing.T) {
	if _, err := Guard(nil); err == nil {
		t.Fatal("Guard(nil) = nil error, want a refusal")
	}
}

// TestGuardDeniesNilAttributes covers the shape the framework never produces
// but the interface allows.
func TestGuardDeniesNilAttributes(t *testing.T) {
	delegate := &recorder{decision: authorizer.DecisionAllow}
	guard, err := Guard(delegate)
	if err != nil {
		t.Fatalf("Guard: %v", err)
	}
	if decision, _, _ := guard.Authorize(context.Background(), nil); decision != authorizer.DecisionDeny {
		t.Errorf("decision = %v, want deny", decision)
	}
	if delegate.calls != 0 {
		t.Errorf("delegate saw %d reviews, want none", delegate.calls)
	}
}

// TestValidateAlwaysAllowPaths pins that an operator cannot quietly write a
// value of --authorization-always-allow-paths that reaches into this service's
// own API group.
func TestValidateAlwaysAllowPaths(t *testing.T) {
	ok := [][]string{
		{"/healthz", "/readyz", "/livez", "/metrics"},
		{"healthz"},
		{"/metrics/slis"},
		{"/apis/telemetry.miloapis.com"},
	}
	for _, paths := range ok {
		if err := ValidateAlwaysAllowPaths(paths); err != nil {
			t.Errorf("ValidateAlwaysAllowPaths(%v) = %v, want nil", paths, err)
		}
	}

	bad := [][]string{
		{"/apis/o11y.miloapis.com"},
		{"/apis/o11y.miloapis.com/v1alpha1/logs/loki/api/v1/query"},
		{"/apis/*"},
		{"/*"},
		{"/healthz", "/apis/o11y.miloapis.com/v1alpha1"},
	}
	for _, paths := range bad {
		if err := ValidateAlwaysAllowPaths(paths); err == nil {
			t.Errorf("ValidateAlwaysAllowPaths(%v) = nil, want an error", paths)
		}
	}
}
