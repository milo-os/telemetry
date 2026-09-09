// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"k8s.io/apiserver/pkg/endpoints/request"

	"go.datum.net/o11y/queryapi/internal/authz"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// APIPrefix is the path kube-aggregator forwards to this service under.
// handler_proxy.go rewrites the host and leaves the path alone, so a request
// Milo received at
// /projects/{id}/control-plane/apis/o11y.miloapis.com/v1alpha1/logs/... arrives
// here as everything from /apis onwards.
const APIPrefix = "/apis/" + authz.APIGroup + "/" + authz.APIVersion

// route is one entry in the table that describes everything this service
// serves. The same entry registers the handler, names the metric label and
// supplies the permission the request is reviewed for, so a route cannot exist
// without a permission and a permission cannot drift from the route it guards.
type route struct {
	// pattern is an http.ServeMux pattern, method included.
	pattern    string
	permission authz.Permission
	handler    http.HandlerFunc
}

// router is the tenant surface: the mux that serves it and the permission each
// of its patterns requires.
//
// Authorization resolves a request through this same mux rather than by
// re-parsing the path (see requestInfoResolver), so the permission that is
// reviewed always belongs to the handler that will run. There is no second
// matcher that could disagree with the first.
type router struct {
	mux         *http.ServeMux
	permissions map[string]authz.Permission
}

// newRouter builds the tenant mux.
func newRouter(logger *slog.Logger, store storage.LogStore, cfg Config) (*router, error) {
	h := &handler{logger: logger, store: store, cfg: cfg}

	// Log endpoints mimic Loki's HTTP API, metric endpoints Prometheus's.
	// /logs and /metrics scope each signal to its own resource segment, which
	// is also the resource each is reviewed against.
	routes := []route{
		{"GET " + APIPrefix + "/logs/loki/api/v1/query", authz.Permission{Resource: "logs", Verb: "query"}, h.lokiQuery},
		{"GET " + APIPrefix + "/logs/loki/api/v1/query_range", authz.Permission{Resource: "logs", Verb: "query"}, h.lokiQueryRange},
		{"GET " + APIPrefix + "/logs/loki/api/v1/labels", authz.Permission{Resource: "logs", Verb: "getLabels"}, h.lokiLabelNames},
		{"GET " + APIPrefix + "/logs/loki/api/v1/label/{name}/values", authz.Permission{Resource: "logs", Verb: "getLabels"}, h.lokiLabelValues},
		{"GET " + APIPrefix + "/logs/loki/api/v1/series", authz.Permission{Resource: "logs", Verb: "getSeries"}, h.lokiSeries},

		// POST-only, form-encoded, matching Grafana's Prometheus datasource.
		{"POST " + APIPrefix + "/metrics/api/v1/query", authz.Permission{Resource: "metrics", Verb: "query"}, h.promQuery},
		{"POST " + APIPrefix + "/metrics/api/v1/query_range", authz.Permission{Resource: "metrics", Verb: "query"}, h.promQueryRange},
		{"GET " + APIPrefix + "/metrics/api/v1/labels", authz.Permission{Resource: "metrics", Verb: "getLabels"}, h.promLabelNames},
		{"GET " + APIPrefix + "/metrics/api/v1/label/{name}/values", authz.Permission{Resource: "metrics", Verb: "getLabels"}, h.promLabelValues},
		{"GET " + APIPrefix + "/metrics/api/v1/series", authz.Permission{Resource: "metrics", Verb: "getSeries"}, h.promSeries},
	}

	r := &router{mux: http.NewServeMux(), permissions: make(map[string]authz.Permission, len(routes))}
	for _, rt := range routes {
		if !authz.Known(rt.permission) {
			// A permission outside the vocabulary would be reviewed as
			// something no role grants, so every caller would see a 403 that
			// looks like a policy problem. Fail at startup instead.
			return nil, fmt.Errorf("route %q requires %s.%s, which is not a known permission",
				rt.pattern, rt.permission.Resource, rt.permission.Verb)
		}
		r.permissions[rt.pattern] = rt.permission
		r.mux.Handle(rt.pattern, traceRoute(rt.pattern, instrumentRoute(rt.pattern, rt.handler)))
	}
	return r, nil
}

// requestInfoResolver returns the resolver the apiserver's WithRequestInfo
// filter runs, and with it the attributes its WithAuthorization filter reviews.
//
// Under this service's own group nothing is delegated: a path either matches
// the route table, in which case it is described as a resource request for the
// permission that route requires, or it is described as a non-resource request
// that authz's gate denies. The stock resolver would happily read
// /apis/o11y.miloapis.com/v1alpha1/logs/loki/api/v1/tail as "get logs" and
// send that to a control plane; nothing here gives it the chance.
//
// Everything outside the group -- /healthz, /metrics, /apis, /version -- goes
// to the stock resolver, because those are the paths --authorization-always-
// allow-paths is written against and it matches on the path it produces.
func (r *router) requestInfoResolver(fallback request.RequestInfoResolver) request.RequestInfoResolver {
	return resolver{router: r, fallback: fallback}
}

type resolver struct {
	router   *router
	fallback request.RequestInfoResolver
}

func (rr resolver) NewRequestInfo(req *http.Request) (*request.RequestInfo, error) {
	const groupPrefix = "/apis/" + authz.APIGroup
	path := req.URL.Path
	if path != groupPrefix && !strings.HasPrefix(path, groupPrefix+"/") {
		return rr.fallback.NewRequestInfo(req)
	}

	// http.ServeMux is the matcher, so the pattern found here is exactly the
	// pattern that will serve the request -- including its method, its
	// {name} wildcard and its path cleaning. A path the mux would redirect or
	// answer with 405 reports no pattern, and is therefore unmapped.
	if _, pattern := rr.router.mux.Handler(req); pattern != "" {
		if permission, ok := rr.router.permissions[pattern]; ok {
			return &request.RequestInfo{
				IsResourceRequest: true,
				Path:              path,
				Verb:              permission.Verb,
				APIPrefix:         "apis",
				APIGroup:          authz.APIGroup,
				APIVersion:        authz.APIVersion,
				Resource:          permission.Resource,
			}, nil
		}
	}

	// Unmapped: the group's discovery documents, and anything under the group
	// that no route serves. Described as a non-resource request so that it
	// carries no resource or verb a control plane could be asked about, and
	// denied by authz's gate on the way through. Even if an operator opened it
	// with --authorization-always-allow-paths, the tenant mux serves nothing
	// here, so there is no handler behind it to reach.
	return &request.RequestInfo{
		IsResourceRequest: false,
		Path:              path,
		Verb:              strings.ToLower(req.Method),
	}, nil
}

// canonicalPath rewrites the two shorter forms this service is reached by onto
// the aggregator's own, so that one path is matched, reviewed and served.
//
//   - Grafana's Loki datasource omits the datasource URL's subpath and sends a
//     bare /loki/api/v1/...
//   - /v1alpha1/... is the form queryapi assumed before it served discovery,
//     from when Milo's ProjectRouter was thought to consume /apis/{group} too.
//     It is kept so a datasource configured against the old assumption keeps
//     working.
//
// Segments are matched whole and only at the head of the path: a lookalike
// further in is client-controlled data -- a label name, a label value -- and
// must survive untouched.
func canonicalPath(path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")

	switch {
	case len(segs) >= 3 && segs[0] == "loki" && segs[1] == "api" && segs[2] == "v1":
		segs = append([]string{"apis", authz.APIGroup, authz.APIVersion, "logs"}, segs...)
	case len(segs) >= 2 && segs[0] == authz.APIVersion && (segs[1] == "logs" || segs[1] == "metrics"):
		segs = append([]string{"apis", authz.APIGroup}, segs...)
	default:
		return path
	}
	return "/" + strings.Join(segs, "/")
}

// withCanonicalPaths normalises the request path before anything else looks at
// it.
//
// It has to be the outermost filter. Authorization reads the path the route
// table was matched against, so rewriting after WithRequestInfo would review
// one path and serve another -- which is the whole class of bug this ordering
// exists to prevent.
func withCanonicalPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := canonicalPath(r.URL.Path)
		if path == r.URL.Path {
			next.ServeHTTP(w, r)
			return
		}

		// Clone preserves the query string while rewriting the path. RawPath
		// is cleared because it no longer describes Path.
		r2 := r.Clone(r.Context())
		r2.URL.Path = path
		r2.URL.RawPath = ""
		next.ServeHTTP(w, r2)
	})
}
