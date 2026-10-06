// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClient(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/cp"+APIPath) {
		case "/labels":
			_, _ = w.Write([]byte(`{"status":"success","data":["service_name"]}`))
			return
		case "/label/service_name/values":
			_, _ = w.Write([]byte(`{"status":"success","data":["api"]}`))
			return
		case "/query_range":
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotQuery = r.URL.Query()
		switch r.URL.Query().Get("query") {
		case "denied":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"forbidden by policy"}`))
		case "bad":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"logql: unexpected token"}`))
		case "unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "matrix":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
				{"stream":{"i":"a"},"values":[["3","a3"],["1","a1"]]},
				{"stream":{"i":"b"},"values":[["2","b2"]]}]}}`))
		}
	}))
	defer srv.Close()

	// The first token is stale, so the client must refresh it after a 401.
	tokens := []string{"stale", "fresh"}
	c := New(srv.URL+"/cp/", WithHTTPClient(srv.Client()), WithToken(func() (string, error) {
		tok := tokens[0]
		tokens = tokens[1:]
		return tok, nil
	}))
	ctx := context.Background()
	start := time.Unix(0, 1_000_000_123)

	entries, err := c.QueryRange(ctx, Query{Query: "{a}", Start: start, End: start, Limit: 10, Direction: Forward})
	if err != nil {
		t.Fatalf("QueryRange() = %v", err)
	}
	if got, want := texts(entries), []string{"a1", "b2", "a3"}; !slices.Equal(got, want) {
		t.Errorf("QueryRange() lines = %v, want %v merged oldest first", got, want)
	}
	if got := gotQuery.Get("start"); got != "1000000123" {
		t.Errorf("start = %s, want nanoseconds", got)
	}

	if names, err := c.LabelNames(ctx, start, start); err != nil || !slices.Equal(names, []string{"service_name"}) {
		t.Errorf("LabelNames() = %v, %v", names, err)
	}
	if values, err := c.LabelValues(ctx, "service_name", start, start); err != nil || !slices.Equal(values, []string{"api"}) {
		t.Errorf("LabelValues() = %v, %v", values, err)
	}

	tests := []struct {
		query     string
		permanent bool
		contains  []string
	}{
		{"denied", true, []string{"403", "forbidden by policy"}},
		{"bad", true, []string{"400", "logql: unexpected token"}},
		{"unavailable", false, []string{"503"}},
	}
	for _, tt := range tests {
		_, err := c.QueryRange(ctx, Query{Query: tt.query})
		if err == nil || Permanent(err) != tt.permanent {
			t.Errorf("QueryRange(%s) = %v, want permanent=%v", tt.query, err, tt.permanent)
			continue
		}
		for _, s := range tt.contains {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("QueryRange(%s) = %q, want it to mention %q", tt.query, err, s)
			}
		}
	}

	// The reason is lifted out of whichever envelope carried it, not dumped raw.
	for q, want := range map[string]string{"denied": "forbidden by policy", "bad": "logql: unexpected token"} {
		_, err := c.QueryRange(ctx, Query{Query: q})
		var ae *APIError
		if !asAPIError(err, &ae) || ae.Detail != want {
			t.Errorf("QueryRange(%s) detail = %+v, want %q", q, ae, want)
		}
	}

	_, err = c.QueryRange(ctx, Query{Query: "matrix"})
	if err == nil || !strings.Contains(err.Error(), "expected streams") {
		t.Errorf("QueryRange(matrix) = %v, want a result type error", err)
	}
}

// A client-go HTTP client authenticates in its transport, so the logs client
// must not demand a token of its own.
func TestClientWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization = %q, want none", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := New(srv.URL, WithHTTPClient(srv.Client())).QueryRange(context.Background(), Query{Query: "{a}"})
	var ae *APIError
	if !asAPIError(err, &ae) || ae.StatusCode != http.StatusUnauthorized || !strings.Contains(ae.Hint(), "datumctl login") {
		t.Errorf("QueryRange() = %v, want a 401 with a login hint and no retry", err)
	}
}

func TestProjectURL(t *testing.T) {
	for host, want := range map[string]string{
		"api.datum.net":           "https://api.datum.net/apis/resourcemanager.miloapis.com/v1alpha1/projects/p-1/control-plane",
		"http://localhost:8080/":  "http://localhost:8080/apis/resourcemanager.miloapis.com/v1alpha1/projects/p-1/control-plane",
		"https://api.example.com": "https://api.example.com/apis/resourcemanager.miloapis.com/v1alpha1/projects/p-1/control-plane",
	} {
		if got := ProjectURL(host, "p-1"); got != want {
			t.Errorf("ProjectURL(%q) = %s, want %s", host, got, want)
		}
	}
}

func TestEntryKey(t *testing.T) {
	// Entries built by hand, as fakes do, key the same as decoded ones.
	decoded := Entry{Time: time.Unix(1, 0), Labels: map[string]string{"a": "1", "b": "2"}, Line: "x", stream: canonical(map[string]string{"b": "2", "a": "1"})}
	built := Entry{Time: time.Unix(1, 0), Labels: map[string]string{"b": "2", "a": "1"}, Line: "x"}
	if decoded.Key() != built.Key() {
		t.Error("Key() differs between a decoded entry and an equal hand-built one")
	}
	other := built
	other.Labels = map[string]string{"a": "1"}
	if other.Key() == built.Key() {
		t.Error("Key() ignores labels; the same line from two streams would be deduplicated")
	}
}
