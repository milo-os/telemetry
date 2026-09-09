// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"context"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/component-base/metrics/legacyregistry"
)

var authorizationTotal = func() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "queryapi_authorization_total",
		Help: "Authorization outcomes by reviewed permission and decision.",
	}, []string{"resource", "verb", "decision"})
	// The apiserver runtime owns /metrics and serves it from component-base's
	// registry, so queryapi's own counters have to live there too or they are
	// simply not scraped.
	legacyregistry.Registerer().MustRegister(c)
	return c
}()

// GroupPrefix is the path everything this service serves lives under, and the
// path Guard treats as its own.
const GroupPrefix = "/apis/" + APIGroup

// discoveryPaths are the only non-resource paths under GroupPrefix that mean
// anything. /apis/o11y.miloapis.com/v1alpha1 is what kube-aggregator's
// availability controller probes, and an APIService whose probe fails is never
// proxied to at all.
var discoveryPaths = map[string]struct{}{
	GroupPrefix:                    {},
	GroupPrefix + "/" + APIVersion: {},
}

// Guard wraps the apiserver's own authorization chain with the one rule the
// framework cannot know: this service has a closed vocabulary, and nothing
// outside it is reviewed at all.
//
// Under /apis/o11y.miloapis.com exactly two kinds of request are forwarded to
// the delegate: a discovery document, and a resource request naming a
// permission in Permissions. Everything else is denied here. That is what
// keeps an unmapped path -- /logs/loki/api/v1/tail, say -- from being asked
// about as a non-resource URL that Milo might grant broadly, and it is the
// second of two independent reasons such a path cannot reach a handler: the
// route table's request info resolver never describes one as a resource
// request in the first place, and the tenant mux serves nothing there.
//
// Everything outside the group is passed straight through, so system:masters,
// --authorization-always-allow-paths and the SubjectAccessReview itself stay
// the framework's. queryapi has exactly one review path.
func Guard(delegate authorizer.Authorizer) (authorizer.Authorizer, error) {
	if delegate == nil {
		// A nil authorizer makes the framework's WithAuthorization filter a
		// pass-through, which is the one outcome this service must never have.
		return nil, fmt.Errorf("an authorizer is required: queryapi serves nothing unreviewed")
	}
	return &guard{delegate: delegate}, nil
}

type guard struct {
	delegate authorizer.Authorizer
}

var _ authorizer.Authorizer = (*guard)(nil)

func (g *guard) Authorize(ctx context.Context, attrs authorizer.Attributes) (authorizer.Decision, string, error) {
	if attrs == nil {
		return authorizer.DecisionDeny, "no attributes to authorize", nil
	}

	if !ours(attrs) {
		// Probes, /metrics, /apis, /version: the framework's own surface,
		// decided by the framework's own chain.
		return g.delegate.Authorize(ctx, attrs)
	}

	if !attrs.IsResourceRequest() {
		if _, ok := discoveryPaths[attrs.GetPath()]; !ok {
			authorizationTotal.WithLabelValues("", "", "unmapped").Inc()
			return authorizer.DecisionDeny, "queryapi serves no such path", nil
		}
		return g.delegate.Authorize(ctx, attrs)
	}

	permission, ok := permissionFor(attrs)
	if !ok {
		authorizationTotal.WithLabelValues(attrs.GetResource(), attrs.GetVerb(), "unmapped").Inc()
		return authorizer.DecisionDeny, "queryapi serves no such resource", nil
	}

	decision, reason, err := g.delegate.Authorize(ctx, attrs)
	authorizationTotal.WithLabelValues(permission.Resource, permission.Verb, outcome(decision, err)).Inc()
	return decision, reason, err
}

// ours reports whether attrs describes a request to this service's own API
// group, by either of the two shapes the resolver can produce.
func ours(attrs authorizer.Attributes) bool {
	if attrs.IsResourceRequest() {
		return attrs.GetAPIGroup() == APIGroup
	}
	path := attrs.GetPath()
	return path == GroupPrefix || strings.HasPrefix(path, GroupPrefix+"/")
}

func outcome(decision authorizer.Decision, err error) string {
	switch {
	case err != nil:
		// The framework turns this into a 500, not a 200: an unreachable
		// SubjectAccessReview endpoint is an outage, never an allow.
		return "error"
	case decision == authorizer.DecisionAllow:
		return "allow"
	default:
		return "deny"
	}
}
