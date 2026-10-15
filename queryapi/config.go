// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"errors"
	"fmt"
	"time"

	"go.datum.net/o11y/queryapi/internal/storage/prometheus"
)

// Config is what the query handlers themselves need. Everything about serving,
// authentication and authorization lives in Options below, because it belongs
// to the apiserver runtime rather than to a handler.
type Config struct {
	// LogsStorage selects the log backend: "fake" or "clickhouse".
	LogsStorage string

	// MetricsStorage selects the metric backend: "fake" or "prometheus".
	MetricsStorage string

	// PrometheusURL, PrometheusTenancy and PrometheusProjectLabel configure
	// the prometheus backend. See internal/storage/prometheus.
	PrometheusURL          string
	PrometheusTenancy      string
	PrometheusProjectLabel string

	// MaxResponseBytes bounds a metric backend response body.
	MaxResponseBytes int64

	// FakeRate is the fake backend's lines per second.
	FakeRate float64

	// QueryTimeout bounds a single storage call, and the /readyz ping.
	QueryTimeout time.Duration

	// MaxRange bounds a query_range window.
	MaxRange time.Duration

	// DefaultLimit and MaxLimit bound returned rows. Requests above MaxLimit
	// are clamped, not rejected, matching Loki.
	DefaultLimit int
	MaxLimit     int
}

// DefaultConfig returns a Config with production-reasonable defaults.
func DefaultConfig() Config {
	return Config{
		LogsStorage:            "fake",
		MetricsStorage:         "fake",
		PrometheusProjectLabel: "resourcemanager_datumapis_com_project_name",
		MaxResponseBytes:       32 << 20,
		FakeRate:               2,
		QueryTimeout:           30 * time.Second,
		MaxRange:               31 * 24 * time.Hour,
		DefaultLimit:           100,
		MaxLimit:               5000,
	}
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	var errs []error
	if c.DefaultLimit <= 0 {
		errs = append(errs, fmt.Errorf("default-limit must be greater than zero, got %d", c.DefaultLimit))
	}
	if c.MaxLimit <= 0 {
		errs = append(errs, fmt.Errorf("max-limit must be greater than zero, got %d", c.MaxLimit))
	}
	if c.DefaultLimit > 0 && c.MaxLimit > 0 && c.DefaultLimit > c.MaxLimit {
		errs = append(errs, fmt.Errorf("default-limit %d exceeds max-limit %d", c.DefaultLimit, c.MaxLimit))
	}
	if c.QueryTimeout <= 0 {
		errs = append(errs, fmt.Errorf("query-timeout must be greater than zero, got %s", c.QueryTimeout))
	}
	if c.MaxRange <= 0 {
		errs = append(errs, fmt.Errorf("max-range must be greater than zero, got %s", c.MaxRange))
	}
	if c.MaxResponseBytes <= 0 {
		errs = append(errs, fmt.Errorf("max-response-bytes must be greater than zero, got %d", c.MaxResponseBytes))
	}
	switch c.MetricsStorage {
	case "fake":
	case "prometheus":
		if c.PrometheusURL == "" {
			errs = append(errs, errors.New("prometheus-url is required with --metrics-storage=prometheus"))
		}
		if _, err := prometheus.ParseTenancy(c.PrometheusTenancy); err != nil {
			errs = append(errs, err)
		}
	default:
		errs = append(errs, fmt.Errorf("unknown metrics storage backend %q: want fake or prometheus", c.MetricsStorage))
	}
	return errors.Join(errs...)
}
