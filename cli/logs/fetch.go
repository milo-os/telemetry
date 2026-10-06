// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"slices"
	"time"
)

// Pages walks q's window in q.Direction, q.Limit lines at a time (MaxLimit if
// unset), passing fn each page's unseen entries until fn returns false or the
// window is exhausted.
//
// The API truncates window bounds to whole seconds, so each page resumes at
// the second of the previous page's last line and drops the lines from that
// second it already returned. A second holding more than a page of lines
// can't be paged into; the rest of it is skipped.
func Pages(ctx context.Context, querier Querier, q Query, fn func([]Entry) bool) error {
	if q.Limit <= 0 {
		q.Limit = MaxLimit
	}
	if q.Direction == "" {
		q.Direction = Backward
	}
	edge := int64(-1)
	seen := map[string]bool{}

	for {
		page, err := querier.QueryRange(ctx, q)
		if err != nil {
			return err
		}

		fresh := make([]Entry, 0, len(page))
		for _, e := range page {
			if e.Time.Unix() != edge || !seen[e.Key()] {
				fresh = append(fresh, e)
			}
		}
		if len(fresh) > 0 && !fn(fresh) {
			return nil
		}
		if len(page) < q.Limit {
			return nil
		}

		last := page[len(page)-1].Time.Unix()
		if len(fresh) == 0 {
			// A whole page from one already-seen second: step past it.
			if q.Direction == Backward {
				q.End = time.Unix(last, 0)
			} else {
				q.Start = time.Unix(last+1, 0)
			}
			edge, seen = -1, map[string]bool{}
			continue
		}

		if last != edge {
			edge, seen = last, map[string]bool{}
		}
		for _, e := range page {
			if e.Time.Unix() == edge {
				seen[e.Key()] = true
			}
		}
		if q.Direction == Backward {
			q.End = time.Unix(last+1, 0)
		} else {
			q.Start = time.Unix(last, 0)
		}
	}
}

// Tail returns the newest n lines matching query in [start, end), oldest
// first.
func Tail(ctx context.Context, querier Querier, query string, start, end time.Time, n int) ([]Entry, error) {
	if n <= 0 {
		return nil, nil
	}
	var out []Entry
	q := Query{Query: query, Start: start, End: end, Limit: min(n, MaxLimit), Direction: Backward}
	err := Pages(ctx, querier, q, func(page []Entry) bool {
		out = append(out, page...)
		return len(out) < n
	})
	if err != nil {
		return nil, err
	}
	out = out[:min(n, len(out))]
	slices.Reverse(out)
	return out, nil
}

// All passes every line matching query in [start, end) to emit, oldest first.
func All(ctx context.Context, querier Querier, query string, start, end time.Time, emit func(Entry)) error {
	q := Query{Query: query, Start: start, End: end, Limit: MaxLimit, Direction: Forward}
	return Pages(ctx, querier, q, func(page []Entry) bool {
		for _, e := range page {
			emit(e)
		}
		return true
	})
}
