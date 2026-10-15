// SPDX-License-Identifier: AGPL-3.0-only

package prometheus

import (
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/component-base/metrics/legacyregistry"
)

// The apiserver serves /metrics from component-base's registry.
var registry = promauto.With(legacyregistry.Registerer())

var (
	backendRequests = registry.NewCounterVec(prom.CounterOpts{
		Name: "queryapi_metrics_backend_requests_total",
		Help: "Requests sent to the metrics backend, by API endpoint and status code (\"error\" when no response arrived).",
	}, []string{"endpoint", "code"})

	backendDuration = registry.NewHistogramVec(prom.HistogramOpts{
		Name:    "queryapi_metrics_backend_request_duration_seconds",
		Help:    "Metrics backend request latency in seconds, by API endpoint.",
		Buckets: prom.DefBuckets,
	}, []string{"endpoint"})
)
