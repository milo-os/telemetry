// SPDX-License-Identifier: AGPL-3.0-only

package storage_test

import (
	"testing"

	"go.datum.net/o11y/queryapi/internal/storage"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		label      string
		wantTarget string
		wantKind   storage.LabelKind
	}{
		{"service_name", "ServiceName", storage.LabelColumn},
		{"severity", "SeverityText", storage.LabelColumn},
		{"trace_id", "TraceId", storage.LabelColumn},
		{"resource_name", "resource_name", storage.LabelResourceAttribute},
		{"anything_at_all", "anything_at_all", storage.LabelLogAttribute},
		{"http_method", "http_method", storage.LabelLogAttribute},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			target, kind := storage.Resolve(tc.label)
			if target != tc.wantTarget || kind != tc.wantKind {
				t.Errorf("Resolve(%q) = (%q, %v), want (%q, %v)",
					tc.label, target, kind, tc.wantTarget, tc.wantKind)
			}
		})
	}
}

func TestLabelSetKeyIsOrderIndependent(t *testing.T) {
	a := storage.LabelSet{"service_name": "waf", "severity": "ERROR"}
	b := storage.LabelSet{"severity": "ERROR", "service_name": "waf"}
	if a.Key() != b.Key() {
		t.Errorf("Key() differs for equal label sets: %q vs %q", a.Key(), b.Key())
	}

	c := storage.LabelSet{"service_name": "waf"}
	if a.Key() == c.Key() {
		t.Error("Key() collides for different label sets")
	}
}

// TestLabelSetKeyResistsSeparatorForgery guards the grouping key against label
// values that contain whatever bytes Key() uses internally. Attribute values
// are arbitrary log data, so a value must never be able to imitate a
// separator and merge two unrelated streams.
func TestLabelSetKeyResistsSeparatorForgery(t *testing.T) {
	cases := [][2]storage.LabelSet{
		{{"x": "a\x01y\x00b"}, {"x": "a", "y": "b"}},
		{{"x": "a,y=b"}, {"x": "a", "y": "b"}},
		{{"x": "1:a=1:b"}, {"x": "a", "y": "b"}},
		{{"xy": "b"}, {"x": "yb"}},
	}

	for i, pair := range cases {
		if pair[0].Key() == pair[1].Key() {
			t.Errorf("case %d: Key() collides for %v and %v: %q",
				i, pair[0], pair[1], pair[0].Key())
		}
	}
}
