// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"testing"
	"time"
)

func TestParsePromTime(t *testing.T) {
	fallback := time.Unix(42, 0).UTC()
	cases := []struct {
		raw     string
		want    time.Time
		wantErr bool
	}{
		{"", fallback, false},
		{"1760400000", time.Unix(1760400000, 0).UTC(), false},
		{"1760400000.123", time.Unix(1760400000, 123_000_000).UTC(), false},
		{"1760400000.123456789", time.Unix(1760400000, 123_456_789).UTC(), false},
		{"1760400000.1234567891", time.Unix(1760400000, 123_456_789).UTC(), false}, // truncated past ns
		{"1760400000.", time.Unix(1760400000, 0).UTC(), false},
		{"2026-10-14T00:00:00Z", time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), false},
		{"2026-10-14T00:00:00.5Z", time.Date(2026, 10, 14, 0, 0, 0, 500_000_000, time.UTC), false},
		{"2026-10-14T02:00:00+02:00", time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), false},
		{"abc", time.Time{}, true},
		{".5", time.Time{}, true},
		{"1e9", time.Time{}, true},
		{"-5", time.Time{}, true},
	}
	for _, tc := range cases {
		got, err := parsePromTime(tc.raw, fallback)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parsePromTime(%q) = %v, want an error", tc.raw, got)
			}
			continue
		}
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("parsePromTime(%q) = %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
}

func TestParsePromDuration(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"30s", 30 * time.Second, false},
		{"1m", time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"1d", 24 * time.Hour, false},
		{"30", 30 * time.Second, false},
		{"0.5", 500 * time.Millisecond, false},
		{"", 0, true},
		{"-1", 0, true},
		{"NaN", 0, true},
		{"Inf", 0, true},
		{"soon", 0, true},
		{"1e-10", 0, true},
		{"0.0000000001", 0, true},
		{"1e30", 0, true},
		{"300000000000", 0, true},
	}
	for _, tc := range cases {
		got, err := parsePromDuration(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parsePromDuration(%q) = %v, want an error", tc.raw, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parsePromDuration(%q) = %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
}
