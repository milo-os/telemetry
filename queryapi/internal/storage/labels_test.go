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

		// Everything that is not a promoted column is an attribute, wherever
		// it happens to live. Which map holds the key is decided per row by
		// the SQL layer, not by a table here -- the old allowlist advertised
		// resource-only keys through /labels and then read them out of
		// LogAttributes, so they resolved to nothing.
		{"resource_name", "resource_name", storage.LabelAttribute},
		{"k8s_node_name", "k8s_node_name", storage.LabelAttribute},
		{"anything_at_all", "anything_at_all", storage.LabelAttribute},
		{"http_method", "http_method", storage.LabelAttribute},
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

// TestSanitize covers the one rule that maps an OTel attribute key to a LogQL
// label name. LogQL label names cannot contain dots (see logql.scanIdent), and
// Grafana's own LogQL editor rejects them client-side, so a dotted key is not
// queryable under any spelling until it is sanitized.
func TestSanitize(t *testing.T) {
	cases := []struct{ key, want string }{
		{"k8s.node.name", "k8s_node_name"},
		{"milo.project.id", "milo_project_id"},
		{"service.namespace", "service_namespace"},

		// Already-valid names are returned unchanged, so sanitizing twice is
		// the same as sanitizing once. The catalogue, row labels and matcher
		// resolution all apply it, and they must agree.
		{"http_method", "http_method"},
		{"service_name", "service_name"},
		{"", ""},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := storage.Sanitize(tc.key); got != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.key, got, tc.want)
			}
			if again := storage.Sanitize(tc.want); again != tc.want {
				t.Errorf("Sanitize(%q) is not idempotent: %q", tc.want, again)
			}
		})
	}
}

// TestSanitizeCollapsesDistinctKeys pins the lossiness down rather than
// wishing it away: two different OTel keys can name one LogQL label. Nothing
// reverses Sanitize -- the SQL layer compares sanitized keys inside the query
// instead -- so a collision widens a match rather than resolving to one map
// entry, and that has to be a deliberate, tested property.
func TestSanitizeCollapsesDistinctKeys(t *testing.T) {
	if storage.Sanitize("k8s.pod.name") != storage.Sanitize("k8s_pod_name") {
		t.Error("Sanitize should map both spellings onto one label name")
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
