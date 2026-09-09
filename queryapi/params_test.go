// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	now := time.Unix(1767225600, 0).UTC()

	cases := []struct {
		name  string
		raw   string
		want  time.Time
		isErr bool
	}{
		{"empty uses fallback", "", now, false},
		{"rfc3339", "2026-01-01T00:00:00Z", now, false},
		{"relative hours", "1h", now.Add(-time.Hour), false},
		{"relative minutes", "30m", now.Add(-30 * time.Minute), false},
		{"unix seconds", "1767225600", now, false},
		{"unix nanos", "1767225600000000000", now, false},
		{"unix millis", "1767225600000", now, false},
		{"unix micros", "1767225600000000", now, false},
		{"ambiguous digit count", "17672256", time.Time{}, true},
		{"garbage", "not-a-time", time.Time{}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTime(tc.raw, now, now)
			if tc.isErr {
				if err == nil {
					t.Fatalf("parseTime(%q) succeeded, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTime(%q): %v", tc.raw, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("parseTime(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseLimit(t *testing.T) {
	cfg := Config{DefaultLimit: 100, MaxLimit: 5000}

	cases := []struct {
		raw  string
		want int
	}{
		{"", 100},
		{"10", 10},
		{"99999", 5000}, // clamped, matching Loki
		{"0", 100},
		{"-5", 100},
		{"abc", 100},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := parseLimit(tc.raw, cfg); got != tc.want {
				t.Errorf("parseLimit(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseDirection(t *testing.T) {
	if got := parseDirection(""); got != "backward" {
		t.Errorf("default direction = %q, want backward", got)
	}
	if got := parseDirection("forward"); got != "forward" {
		t.Errorf("parseDirection(forward) = %q", got)
	}
	if got := parseDirection("sideways"); got != "backward" {
		t.Errorf("unknown direction = %q, want backward", got)
	}
}
