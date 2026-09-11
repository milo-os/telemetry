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
	// Labels arrives as parallel arrays rather than a map so that a duplicate
	// key -- a sanitized-name collision -- survives the scan.
	var (
		timestamp   time.Time
		body        string
		service     string
		severity    string
		trace       string
		labelKeys   []string
		labelValues []string
	)
	if err := it.rows.Scan(&timestamp, &body, &service, &severity, &trace, &labelKeys, &labelValues); err != nil {
		it.err = err
		return false
	}
	it.row = storage.Row{
		Timestamp: timestamp,
		Labels:    assembleLabels(service, severity, trace, labelKeys, labelValues),
		Line:      body,
	}
	return true
}

func (it *rowIterator) Row() storage.Row { return it.row }

func (it *rowIterator) Err() error { return it.err }

func (it *rowIterator) Close() error { return it.rows.Close() }

// internalLogAttributes exist only so the schema can derive a column from
// them, so they never reach a caller's labels. Keyed by sanitized label name,
// because that is the spelling both the catalogue and a row's labels carry.
var internalLogAttributes = map[string]struct{}{
	storage.Sanitize("telemetry.observed_time_unix_nano"): {},
}

// withoutInternalAttributes filters a label-name catalogue. Its input is
// already sanitized -- buildLabelNamesQuery does that in SQL.
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
// non-empty) plus the row's Labels entries, which the schema already merged and
// normalised -- so every name here is one a matcher accepts.
//
// keys and values are parallel, in Labels' own order, and may repeat a key: a
// sanitized-name collision leaves both source entries in the column. The FIRST
// occurrence wins, because that is what Labels[k] resolves to in SQL, and the
// 000002 migration puts LogAttributes first so the winner is the log
// attribute. Scanning into a map instead would silently take the last, giving
// row labels that disagree with the matchers.
//
// Promoted columns are set before the loop and are not overwritten, so a
// column and an identically named attribute resolve to the column.
func assembleLabels(service, severity, trace string, keys, values []string) storage.LabelSet {
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
	for i, name := range keys {
		if i >= len(values) {
			break
		}
		if _, internal := internalLogAttributes[name]; internal {
			continue
		}
		if _, taken := ls[name]; taken {
			continue
		}
		if values[i] != "" {
			ls[name] = values[i]
		}
	}
	return ls
}
