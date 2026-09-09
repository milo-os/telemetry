// SPDX-License-Identifier: AGPL-3.0-only

package clickhouse

import (
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"go.datum.net/o11y/queryapi/internal/storage"
)

// rowIterator streams rows from a ClickHouse result without intermediate
// buffering; the handler groups them into Loki's envelope as they arrive.
type rowIterator struct {
	rows driver.Rows
	row  storage.Row
	err  error
}

func (it *rowIterator) Next() bool {
	if !it.rows.Next() {
		it.err = it.rows.Err()
		return false
	}

	// ObservedTimestamp is scanned first, matching logsSelect's column order.
	var (
		timestamp time.Time
		body      string
		service   string
		severity  string
		trace     string
		resAttrs  map[string]string
		logAttrs  map[string]string
	)
	if err := it.rows.Scan(&timestamp, &body, &service, &severity, &trace, &resAttrs, &logAttrs); err != nil {
		it.err = err
		return false
	}
	it.row = storage.Row{
		Timestamp: timestamp,
		Labels:    assembleLabels(service, severity, trace, resAttrs, logAttrs),
		Line:      body,
	}
	return true
}

func (it *rowIterator) Row() storage.Row { return it.row }

func (it *rowIterator) Err() error { return it.err }

func (it *rowIterator) Close() error { return it.rows.Close() }

// internalLogAttributes exist only so the schema can derive a column from
// them, so they never reach a caller's labels.
var internalLogAttributes = map[string]struct{}{
	"telemetry.observed_time_unix_nano": {},
}

// withoutInternalAttributes filters a label-name catalogue.
func withoutInternalAttributes(names []string) []string {
	kept := make([]string, 0, len(names))
	for _, n := range names {
		if _, internal := internalLogAttributes[n]; !internal {
			kept = append(kept, n)
		}
	}
	return kept
}

// assembleLabels builds a row's label set from the promoted columns (kept when
// non-empty) plus every non-empty resource and log attribute.
func assembleLabels(service, severity, trace string, resAttrs, logAttrs map[string]string) storage.LabelSet {
	ls := storage.LabelSet{}
	if service != "" {
		ls["service_name"] = service
	}
	if severity != "" {
		ls["severity"] = severity
	}
	if trace != "" {
		ls["trace_id"] = trace
	}
	for k, v := range resAttrs {
		if v != "" {
			ls[k] = v
		}
	}
	for k, v := range logAttrs {
		if _, internal := internalLogAttributes[k]; internal {
			continue
		}
		if v != "" {
			ls[k] = v
		}
	}
	return ls
}
