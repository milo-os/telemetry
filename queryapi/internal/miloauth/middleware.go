// SPDX-License-Identifier: AGPL-3.0-only

package miloauth

import (
	"log/slog"
	"net/http"

	"k8s.io/apiserver/pkg/endpoints/request"
)

// Middleware puts the project the caller is acting in onto the context, so the
// storage layer can bind it to the ClickHouse telemetry_project_id setting.
//
// It wraps only the tenant routes. Probes, /metrics and the discovery
// documents carry no tenancy, and the framework has already authenticated
// every request by the time anything registered on the mux runs.
//
// Reaching a handler at all means authorization allowed the request, and
// authorization is the same Resolve call on the same verified user.Info -- so
// the project the query is bound to is the project the SubjectAccessReview was
// evaluated for. The 403 below is therefore unreachable in the assembled
// server; it is here so that a future reordering fails closed rather than
// serving one project's telemetry under another's grant.
func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := request.UserFrom(r.Context())
		if !ok {
			logger.Error("tenant route reached without an authenticated caller", "path", r.URL.Path)
			forbidden(w, "request is not authenticated")
			return
		}
		id, ok := Resolve(u)
		if !ok {
			logger.Error("tenant route reached without a project", "path", r.URL.Path, "user", u.GetName())
			forbidden(w, "request is not scoped to a project")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithProject(r.Context(), id)))
	})
}

// forbidden writes the Loki/Prometheus-style error envelope openapi.yaml's
// ErrorResponse describes, so a caller that gets this far sees the same shape
// the handlers use.
func forbidden(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"status":"error","error":"` + message + `"}`))
}
