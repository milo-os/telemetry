// SPDX-License-Identifier: AGPL-3.0-only

// Tracing for the queryapi service

package queryapi

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// traceRoute wraps handler in otelhttp instrumentation (matching
// milo-apiserver's config.go). It uses the global TracerProvider/Propagator;
// set one via otel.SetTracerProvider to export, otherwise this is a no-op.
func traceRoute(route string, handler http.HandlerFunc) http.Handler {
	return otelhttp.NewHandler(handler, route)
}
