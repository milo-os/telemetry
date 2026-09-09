// SPDX-License-Identifier: AGPL-3.0-only

// Metrics for the queryapi service

package queryapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/component-base/metrics/legacyregistry"
)

// The apiserver runtime owns /metrics and serves it from component-base's
// registry rather than prometheus's default one, so queryapi's own metrics
// have to be registered there or they are simply not scraped.
var registry = promauto.With(legacyregistry.Registerer())

var (
	requestsTotal = registry.NewCounterVec(prometheus.CounterOpts{
		Name: "queryapi_http_requests_total",
		Help: "Total HTTP requests handled, by route and status code.",
	}, []string{"route", "code"})

	requestDuration = registry.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "queryapi_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds, by route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route"})
)

// instrumentRoute wraps handler with request-count and latency metrics labeled
// by the mux pattern (e.g. "GET /v1/logs"), not the raw path, so per-project
// paths don't create unbounded label cardinality.
func instrumentRoute(route string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		handler(rec, r)
		requestDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
		requestsTotal.WithLabelValues(route, strconv.Itoa(rec.status)).Inc()
	}
}

// statusRecorder captures the status code a handler wrote; http.ResponseWriter
// has no getter for it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
