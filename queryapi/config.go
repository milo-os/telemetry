// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"errors"
	"fmt"
	"time"
)

// Config is what the query handlers themselves need. Everything about serving,
// authentication and authorization lives in Options below, because it belongs
// to the apiserver runtime rather than to a handler.
type Config struct {
	// Storage selects the backend: "fake" or "clickhouse".
	Storage string

	// FakeRate is the fake backend's lines per second.
	FakeRate float64

	// QueryTimeout bounds a single storage call, and the /readyz ping.
	QueryTimeout time.Duration

	// DefaultLimit and MaxLimit bound returned rows. Requests above MaxLimit
	// are clamped, not rejected, matching Loki.
	DefaultLimit int
	MaxLimit     int
}

// DefaultConfig returns a Config with production-reasonable defaults.
func DefaultConfig() Config {
	return Config{
		Storage:      "fake",
		FakeRate:     2,
		QueryTimeout: 30 * time.Second,
		DefaultLimit: 100,
		MaxLimit:     5000,
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
	return errors.Join(errs...)
}
