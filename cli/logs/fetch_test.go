// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// fakeQuerier serves entries the way the query API does: over [start, end)
// with both bounds truncated to the second, ordered by direction, cut at the
// limit. Queries are recorded but not evaluated. If pages is set, its pages
// are returned in order instead.
type fakeQuerier struct {
	entries []Entry
	pages   [][]Entry
	errs    []error // returned first, in order; nil entries fall through

	queries []Query
}

func (f *fakeQuerier) QueryRange(_ context.Context, q Query) ([]Entry, error) {
	f.queries = append(f.queries, q)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	if f.pages != nil {
		if len(f.pages) == 0 {
			return nil, nil
		}
		p := f.pages[0]
		f.pages = f.pages[1:]
		return p, nil
	}
	start := q.Start.Truncate(time.Second)
	end := q.End.Truncate(time.Second)
	var out []Entry
	for _, e := range f.entries {
		if !e.Time.Before(start) && e.Time.Before(end) {
			out = append(out, e)
		}
	}
	Sort(out, q.Direction)
	return out[:min(q.Limit, len(out))], nil
}

func line(ns int64, text string) Entry {
	return Entry{Time: time.Unix(0, ns), Line: text, Labels: map[string]string{}}
}

// lines builds n seconds of entries, perSecond distinct lines in each.
func lines(n, perSecond int) []Entry {
	var out []Entry
	for i := 1; i <= n; i++ {
		for j := range perSecond {
			ns := int64(i)*time.Second.Nanoseconds() + int64(j)*1000
			out = append(out, line(ns, fmt.Sprintf("%d.%d", i, j)))
		}
	}
	return out
}

func texts(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Line
	}
	return out
}

func asAPIError(err error, target **APIError) bool {
	return errors.As(err, target)
}

var (
	epoch = time.Unix(0, 0)
	far   = time.Unix(1<<20, 0)
)

func TestPages(t *testing.T) {
	sec := time.Second.Nanoseconds()
	both := []Direction{Forward, Backward}
	tests := []struct {
		name     string
		entries  []Entry
		pageSize int
		dirs     []Direction
		want     []string // forward order; nil means every entry
	}{
		{name: "exact multiple of the page", entries: lines(9, 1), pageSize: 3, dirs: both},
		{name: "a second straddles each page boundary", entries: lines(7, 2), pageSize: 3, dirs: both},
		{name: "several seconds per page", entries: lines(20, 3), pageSize: 7, dirs: both},
		{
			// Second-truncated bounds can't reach past a page's worth of one
			// second, so the rest of it is skipped.
			name:     "a second wider than a page",
			entries:  append(lines(1, 4), line(2*sec, "after")),
			pageSize: 2,
			dirs:     []Direction{Forward},
			want:     []string{"1.0", "1.1", "after"},
		},
	}
	for _, tt := range tests {
		for _, dir := range tt.dirs {
			t.Run(tt.name+"/"+string(dir), func(t *testing.T) {
				var got []Entry
				q := Query{Query: "{}", Start: epoch, End: far, Limit: tt.pageSize, Direction: dir}
				err := Pages(context.Background(), &fakeQuerier{entries: tt.entries}, q, func(p []Entry) bool {
					got = append(got, p...)
					return true
				})
				if err != nil {
					t.Fatal(err)
				}
				want := tt.want
				if want == nil {
					want = texts(tt.entries)
				}
				if dir == Backward {
					want = slices.Clone(want)
					slices.Reverse(want)
				}
				if !slices.Equal(texts(got), want) {
					t.Errorf("Pages() = %v, want %v", texts(got), want)
				}
			})
		}
	}
}

func TestTail(t *testing.T) {
	q := &fakeQuerier{entries: lines(10, 1)}
	got, err := Tail(context.Background(), q, "{}", epoch, far, 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"7.0", "8.0", "9.0", "10.0"}; !slices.Equal(texts(got), want) {
		t.Errorf("Tail() = %v, want %v", texts(got), want)
	}
	if q.queries[0].Direction != Backward || q.queries[0].Limit != 4 {
		t.Errorf("Tail() asked %+v, want backward for 4 lines", q.queries[0])
	}
}

func TestAll(t *testing.T) {
	q := &fakeQuerier{entries: lines(5, 2)}
	var got []Entry
	if err := All(context.Background(), q, "{}", epoch, far, func(e Entry) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(texts(got), texts(lines(5, 2))) {
		t.Errorf("All() = %v, want every line oldest first", texts(got))
	}
}
