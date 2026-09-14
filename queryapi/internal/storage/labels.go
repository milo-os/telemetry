// SPDX-License-Identifier: AGPL-3.0-only

package storage

import "strings"

// LabelKind is where a LogQL label lives in the logs table.
type LabelKind int

const (
	LabelColumn LabelKind = iota
	LabelAttribute
)

// columns are the promoted labels backed by real columns. Everything else is
// an attribute lookup, so new attributes are queryable without a code change.
var columns = map[string]string{
	"service_name": "ServiceName",
	"severity":     "SeverityText",
	"trace_id":     "TraceId",
}

// Sanitize maps an OTel attribute key to its LogQL label name. LogQL label
// names cannot contain dots (logql.scanIdent), and Grafana's LogQL editor
// rejects them client-side, so an unsanitized key is not queryable under any
// spelling.
//
// It is lossy on purpose: k8s.pod.name and k8s_pod_name name one label. There
// is no inverse, so nothing reverses it -- the catalogue, row labels and
// matcher resolution all apply it in the same direction, and the SQL layer
// compares sanitized keys inside the query rather than reconstructing one.
func Sanitize(key string) string { return strings.ReplaceAll(key, ".", "_") }

// Resolve maps a LogQL label to its storage location.
//
// A label is either a promoted column or an attribute. Which of the two
// attribute maps holds an attribute is not decided here: it is a per-row fact,
// so the SQL layer searches both. The allowlist this replaced could not know
// that -- it sent every unlisted label to LogAttributes, while the catalogue
// advertised the keys of both maps, so every resource-only key was offered and
// then read from the wrong map.
//
// The returned target is a sanitized map KEY for LabelAttribute: bind it as a
// query parameter, never concatenate it into SQL text.
func Resolve(label string) (target string, kind LabelKind) {
	if col, ok := columns[label]; ok {
		return col, LabelColumn
	}
	return Sanitize(label), LabelAttribute
}
