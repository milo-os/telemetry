// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"errors"
	"testing"
	"time"

	"go.datum.net/o11y/queryapi/internal/storage"
)

type stubIterator struct {
	rows     []storage.Row
	i        int
	err      error
	closeErr error
}

func (s *stubIterator) Next() bool {
	if s.i >= len(s.rows) {
		return false
	}
	s.i++
	return true
}

func (s *stubIterator) Row() storage.Row { return s.rows[s.i-1] }
func (s *stubIterator) Err() error       { return s.err }
func (s *stubIterator) Close() error     { return s.closeErr }

func row(ts int64, svc, line string) storage.Row {
	return storage.Row{
		Timestamp: time.Unix(0, ts).UTC(),
		Labels:    storage.LabelSet{"service_name": svc},
		Line:      line,
	}
}

func TestCollectStreamsGroupsByLabelSet(t *testing.T) {
	iter := &stubIterator{rows: []storage.Row{
		row(3, "waf", "c"),
		row(2, "envoy", "b"),
		row(1, "waf", "a"),
	}}

	streams, err := collectStreams(iter)
	if err != nil {
		t.Fatalf("collectStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("got %d streams, want 2: %+v", len(streams), streams)
	}

	// First stream is the first label set encountered, with its rows in
	// iteration order and nanosecond timestamps as strings.
	if streams[0].Stream["service_name"] != "waf" {
		t.Errorf("stream 0 = %v, want service_name=waf", streams[0].Stream)
	}
	if len(streams[0].Values) != 2 {
		t.Fatalf("stream 0 has %d values, want 2", len(streams[0].Values))
	}
	if streams[0].Values[0][0] != "3" || streams[0].Values[0][1] != "c" {
		t.Errorf("stream 0 value 0 = %v, want [3 c]", streams[0].Values[0])
	}
	if streams[0].Values[1][0] != "1" {
		t.Errorf("stream 0 value 1 timestamp = %q, want \"1\"", streams[0].Values[1][0])
	}
	if len(streams[1].Values) != 1 || streams[1].Values[0][1] != "b" {
		t.Errorf("stream 1 = %+v, want one value \"b\"", streams[1])
	}
}

func TestCollectStreamsEmptyIsNotAnError(t *testing.T) {
	streams, err := collectStreams(&stubIterator{})
	if err != nil {
		t.Fatalf("collectStreams: %v", err)
	}
	if len(streams) != 0 {
		t.Errorf("got %d streams, want 0", len(streams))
	}
}

func TestCollectStreamsSurfacesDrainError(t *testing.T) {
	want := errors.New("boom")
	if _, err := collectStreams(&stubIterator{err: want}); !errors.Is(err, want) {
		t.Fatalf("collectStreams err = %v, want it to wrap %v", err, want)
	}
}

// TestCollectStreamsSurfacesCloseError pins the errors.Join contract: a
// failing Close must not be swallowed, or a mid-stream failure would look
// like a successful truncated response.
func TestCollectStreamsSurfacesCloseError(t *testing.T) {
	want := errors.New("close failed")
	if _, err := collectStreams(&stubIterator{closeErr: want}); !errors.Is(err, want) {
		t.Fatalf("collectStreams err = %v, want it to wrap %v", err, want)
	}
}

func TestCollectStreamsJoinsBothErrors(t *testing.T) {
	drain := errors.New("drain failed")
	closeErr := errors.New("close failed")
	_, err := collectStreams(&stubIterator{err: drain, closeErr: closeErr})
	if !errors.Is(err, drain) || !errors.Is(err, closeErr) {
		t.Fatalf("collectStreams err = %v, want it to wrap both %v and %v", err, drain, closeErr)
	}
}
