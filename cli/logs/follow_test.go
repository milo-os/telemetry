// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFollowerAdmit(t *testing.T) {
	sec := time.Second.Nanoseconds()
	f := Follower{floor: 2 * sec}

	got := f.admit([]Entry{line(1*sec, "left out by the tail"), line(2*sec, "b"), line(3*sec, "c")})
	if want := []string{"b", "c"}; !slices.Equal(texts(got), want) {
		t.Errorf("admit() = %v, want %v", texts(got), want)
	}
	// An overlapping poll re-returns c.
	got = f.admit([]Entry{line(3*sec, "c"), line(4*sec, "d")})
	if want := []string{"d"}; !slices.Equal(texts(got), want) {
		t.Errorf("admit() = %v, want %v", texts(got), want)
	}

	// Keys live for the overlap plus the second a poll's start is truncated by.
	f.admit([]Entry{line(4*sec+FollowOverlap.Nanoseconds()+sec, "e")})
	if _, ok := f.seen[line(4*sec, "d").Key()]; !ok {
		t.Error("forgot d while a truncated poll could still return it")
	}
	if _, ok := f.seen[line(3*sec, "c").Key()]; ok {
		t.Error("kept c past the overlap")
	}
}

func TestFollowerBacklog(t *testing.T) {
	sec := time.Second.Nanoseconds()
	var f Follower
	f.Backlog(line(5*sec, "shown"), line(6*sec, "newest shown"))

	// The first poll's overlap reaches back past the backlog: lines the tail
	// left out and lines it printed must both stay out.
	got := f.admit([]Entry{line(1*sec, "older than the backlog"), line(6*sec, "newest shown"), line(7*sec, "new")})
	if want := []string{"new"}; !slices.Equal(texts(got), want) {
		t.Errorf("admit() after Backlog = %v, want %v", texts(got), want)
	}
}

func TestFollowerRun(t *testing.T) {
	defer func(d time.Duration) { followErrorInterval = d }(followErrorInterval)
	followErrorInterval = time.Millisecond

	now := time.Now().UnixNano()
	q := &fakeQuerier{
		errs: []error{
			errors.New("connection reset"),
			nil,
			&APIError{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Detail: "denied"},
		},
		pages: [][]Entry{{line(now, "back")}},
	}
	var status strings.Builder
	var printed []string
	f := Follower{Querier: q, Query: "{}", Status: &status}
	err := f.Run(context.Background(), func(e Entry) { printed = append(printed, e.Line) })

	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("Run() = %v, want the 403 to end it", err)
	}
	if want := []string{"back"}; !slices.Equal(printed, want) {
		t.Errorf("printed %v, want %v", printed, want)
	}
	if s := status.String(); !strings.Contains(s, "retrying") || !strings.Contains(s, "Reconnected") {
		t.Errorf("status = %q, want the outage and recovery reported", s)
	}
	if got, want := q.queries[2].Start, time.Unix(0, now).Add(-FollowOverlap); !got.Equal(want) {
		t.Errorf("poll start = %v, want %v", got, want)
	}
}

func TestFollowerRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := &fakeQuerier{}
	f := Follower{Querier: q, Query: "{}"}
	done := make(chan error)
	go func() { done <- f.Run(ctx, func(Entry) {}) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancel")
	}
}
