// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.datum.net/o11y/queryapi/internal/storage"
)

// parseTime accepts RFC 3339, a Unix timestamp in seconds or nanoseconds, or
// a relative duration meaning "that long before now". Empty returns fallback.
func parseTime(raw string, now, fallback time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		switch len(raw) {
		case 10, 11:
			return time.Unix(n, 0).UTC(), nil
		case 13:
			return time.UnixMilli(n).UTC(), nil
		case 16:
			return time.UnixMicro(n).UTC(), nil
		case 19:
			return time.Unix(0, n).UTC(), nil
		default:
			return time.Time{}, fmt.Errorf(
				"ambiguous epoch timestamp %q: expected 10 (s), 13 (ms), 16 (us) or 19 (ns) digits", raw)
		}
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", raw)
}

// parseLimit clamps to MaxLimit rather than rejecting, matching Loki.
func parseLimit(raw string, cfg Config) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return cfg.DefaultLimit
	}
	if n > cfg.MaxLimit {
		return cfg.MaxLimit
	}
	return n
}

func parseDirection(raw string) storage.Direction {
	if strings.TrimSpace(raw) == string(storage.DirectionForward) {
		return storage.DirectionForward
	}
	return storage.DirectionBackward
}
