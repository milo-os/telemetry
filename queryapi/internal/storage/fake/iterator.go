// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"context"
	"time"

	"go.datum.net/o11y/queryapi/internal/logql"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// iterator walks tick indexes lazily in the requested direction, so a limit
// costs work proportional to the rows returned rather than to the window.
type iterator struct {
	ctx      context.Context
	interval time.Duration
	query    storage.LogQuery

	next, last int64 // inclusive tick bounds in iteration order
	step       int64
	emitted    int
	current    storage.Row
	err        error
}

func newIterator(ctx context.Context, interval time.Duration, q storage.LogQuery) *iterator {
	first := ceilDiv(q.Range.Start.UnixNano(), int64(interval))
	final := (q.Range.End.UnixNano() - 1) / int64(interval)

	it := &iterator{ctx: ctx, interval: interval, query: q}
	if q.Direction == storage.DirectionForward {
		it.next, it.last, it.step = first, final, 1
	} else {
		it.next, it.last, it.step = final, first, -1
	}
	return it
}

func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b > 0 {
		q++
	}
	return q
}

func (it *iterator) Next() bool {
	if it.err != nil || (it.query.Limit > 0 && it.emitted >= it.query.Limit) {
		return false
	}

	for it.inRange() {
		if err := it.ctx.Err(); err != nil {
			it.err = err
			return false
		}

		row := rowAt(it.next, it.interval)
		it.next += it.step

		if !matchesLabels(it.query.Matchers, row.Labels) || !matchesLine(it.query.Filters, row.Line) {
			continue
		}
		it.current = row
		it.emitted++
		return true
	}
	return false
}

func (it *iterator) inRange() bool {
	if it.step > 0 {
		return it.next <= it.last
	}
	return it.next >= it.last
}

func matchesLine(filters []logql.LineFilter, line string) bool {
	for _, f := range filters {
		if !f.Matches(line) {
			return false
		}
	}
	return true
}

func (it *iterator) Row() storage.Row { return it.current }
func (it *iterator) Err() error       { return it.err }
func (it *iterator) Close() error     { return nil }
