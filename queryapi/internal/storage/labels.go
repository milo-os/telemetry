// SPDX-License-Identifier: AGPL-3.0-only

package storage

// LabelKind is where a LogQL label lives in the logs table.
type LabelKind int

const (
	LabelColumn LabelKind = iota
	LabelResourceAttribute
	LabelLogAttribute
)

// columns are the promoted labels backed by real columns. Everything else is
// an attribute lookup, so new attributes are queryable without a code change.
var columns = map[string]string{
	"service_name": "ServiceName",
	"severity":     "SeverityText",
	"trace_id":     "TraceId",
}

// resourceAttributes are labels backed by ResourceAttributes rather than
// LogAttributes -- resource identity is resource-scoped in the logs table.
var resourceAttributes = map[string]bool{
	"resource_name": true,
}

// Resolve maps a LogQL label to its storage location. Unknown labels resolve
// to LabelLogAttribute.
//
// The returned target is a map KEY for attribute kinds: bind it as a query
// parameter, never concatenate it into SQL text. LogQL label names cannot
// contain dots, so OTel keys like http.method are matched with underscores and
// the SQL layer owes the reverse mapping.
func Resolve(label string) (target string, kind LabelKind) {
	if col, ok := columns[label]; ok {
		return col, LabelColumn
	}
	if resourceAttributes[label] {
		return label, LabelResourceAttribute
	}
	return label, LabelLogAttribute
}
