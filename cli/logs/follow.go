// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"fmt"
	"io"
	"time"
)

const (
	// FollowOverlap is how far before the newest printed line each poll
	// starts, to catch lines still being ingested.
	FollowOverlap = 10 * time.Second

	followMinInterval = 500 * time.Millisecond
	followMaxInterval = 2 * time.Second
	followBackoffStep = 250 * time.Millisecond

	// followEndSlack keeps a local clock running behind the collector's from
	// hiding fresh lines.
	followEndSlack = time.Minute
)

// followErrorInterval is a variable so tests can shorten it.
var followErrorInterval = 10 * time.Second

// Follower polls for new lines, remembering what it has emitted so that
// overlapping polls don't repeat it. The query API has no push-based tail yet.
type Follower struct {
	Querier Querier
	Query   string
	// Status receives outage and recovery notices. Nil discards them.
	Status io.Writer

	cursor int64 // newest timestamp emitted
	seen   map[string]int64

	// floor drops lines older than the backlog, which a tail left out; the
	// first poll's overlap would otherwise print them after it.
	floor int64
}

// Backlog records entries already shown before following began, so that
// following neither repeats them nor prints anything older. Call it with each
// backlog entry, oldest first.
func (f *Follower) Backlog(entries ...Entry) {
	f.admit(entries)
	f.floor = f.cursor
}

// Run polls until ctx is done, passing each new line to emit oldest first.
// Without a backlog it starts FollowOverlap before now. Errors retrying cannot
// fix end it; others are reported to Status once and retried.
func (f *Follower) Run(ctx context.Context, emit func(Entry)) error {
	status := f.Status
	if status == nil {
		status = io.Discard
	}
	since := time.Now().Add(-FollowOverlap)
	var interval time.Duration
	failing, skipOverlap := false, false

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}

		start := since
		if f.cursor > 0 {
			start = time.Unix(0, f.cursor)
			if !skipOverlap {
				start = start.Add(-FollowOverlap)
			}
		}
		entries, err := f.Querier.QueryRange(ctx, Query{
			Query:     f.Query,
			Start:     start,
			End:       time.Now().Add(followEndSlack),
			Limit:     MaxLimit,
			Direction: Forward,
		})
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil && Permanent(err):
			return err
		case err != nil:
			if !failing {
				_, _ = fmt.Fprintf(status, "Lost contact with the logs API, retrying: %v\n", err)
				failing = true
			}
			interval = followErrorInterval
			continue
		}
		if failing {
			_, _ = fmt.Fprintln(status, "Reconnected.")
			failing = false
		}

		fresh := f.admit(entries)
		for _, e := range fresh {
			emit(e)
		}

		full := len(entries) >= MaxLimit
		// A full page of already-emitted lines means the overlap alone exceeds
		// a page; step past it instead of refetching it forever.
		skipOverlap = full && len(fresh) == 0
		switch {
		case full:
			interval = 0
		case len(fresh) > 0:
			interval = followMinInterval
		default:
			interval = min(max(interval, followMinInterval)+followBackoffStep, followMaxInterval)
		}
	}
}

// admit returns the entries not emitted before and records them.
func (f *Follower) admit(entries []Entry) []Entry {
	if f.seen == nil {
		f.seen = map[string]int64{}
	}
	var fresh []Entry
	for _, e := range entries {
		ts := e.Time.UnixNano()
		if ts < f.floor {
			continue
		}
		if _, ok := f.seen[e.Key()]; ok {
			continue
		}
		f.seen[e.Key()] = ts
		fresh = append(fresh, e)
		f.cursor = max(f.cursor, ts)
	}
	// A poll starts at cursor-overlap truncated to the second; nothing older
	// comes back.
	cutoff := f.cursor - (FollowOverlap + time.Second).Nanoseconds()
	for k, ts := range f.seen {
		if ts < cutoff {
			delete(f.seen, k)
		}
	}
	return fresh
}
