// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// parsePromTime parses a Prometheus API timestamp: RFC 3339, or Unix seconds
// with an optional fraction. Empty returns fallback.
func parsePromTime(raw string, fallback time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), nil
	}
	if t, ok := parseUnixSeconds(raw); ok {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q: want RFC 3339 or Unix seconds", raw)
}

// parseUnixSeconds parses digits with an optional decimal fraction exactly,
// without a float, so nanoseconds survive. Digits past the ninth are
// truncated.
func parseUnixSeconds(raw string) (time.Time, bool) {
	whole, frac, _ := strings.Cut(raw, ".")
	if whole == "" || !isDigits(whole) || !isDigits(frac) {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	var nsec int64
	if frac != "" {
		nsec, _ = strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
	}
	return time.Unix(sec, nsec).UTC(), true
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parsePromDuration parses a Prometheus duration ("1m", "1h30m", "1d") or
// float seconds ("30", "0.5"). The result is always positive.
func parsePromDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if d, err := model.ParseDuration(raw); err == nil && d > 0 {
		return time.Duration(d), nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, fmt.Errorf("invalid duration %q: want a Prometheus duration or positive seconds", raw)
	}
	ns := math.Round(f * float64(time.Second))
	if ns < 1 || ns >= math.MaxInt64 {
		return 0, fmt.Errorf("duration %q out of range", raw)
	}
	return time.Duration(ns), nil
}
