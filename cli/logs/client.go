// SPDX-License-Identifier: AGPL-3.0-only

// Package logs is a client for the telemetry query API's Loki-compatible log
// endpoints, as served on a project's control plane.
//
// It is shared by every CLI and agent that reads logs back, so that paging,
// following and error reporting behave the same everywhere. Domain packages
// build their own selectors (see package logql) and render entries their own
// way; this package does neither.
package logs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// APIPath is where the Loki-compatible endpoints hang off a control plane.
	// The group, the version and the /logs segment are all load-bearing: the
	// query API reviews the logs resource's permissions against this path.
	APIPath = "/apis/o11y.miloapis.com/v1alpha1/logs/loki/api/v1"

	// MaxLimit is the query API's server-side cap on lines per request.
	MaxLimit = 5000

	requestTimeout = 60 * time.Second

	// tokenTTL bounds token reuse. Following polls several times a second, and
	// a datumctl plugin's token source execs datumctl on every call.
	tokenTTL = time.Minute
)

// Direction orders a query's lines by timestamp.
type Direction string

const (
	Backward Direction = "backward" // newest first
	Forward  Direction = "forward"  // oldest first
)

// Entry is one log line, lifted out of the stream it arrived in.
type Entry struct {
	Time   time.Time         `json:"time"`
	Labels map[string]string `json:"labels"`
	Line   string            `json:"line"`

	stream string // canonical Labels, computed once per stream
}

// Key identifies the entry for de-duplication across overlapping queries.
func (e Entry) Key() string {
	stream := e.stream
	if stream == "" {
		stream = canonical(e.Labels)
	}
	return strconv.FormatInt(e.Time.UnixNano(), 10) + "\x00" + stream + "\x00" + e.Line
}

// Query is one query_range request over [Start, End).
type Query struct {
	Query     string
	Start     time.Time
	End       time.Time
	Limit     int
	Direction Direction
}

// Querier runs range queries. Client implements it; tests substitute fakes.
type Querier interface {
	QueryRange(ctx context.Context, q Query) ([]Entry, error)
}

// TokenFunc returns a bearer token for the next request.
type TokenFunc func() (string, error)

// Client talks to one control plane's logs API.
type Client struct {
	base      string
	http      *http.Client
	token     TokenFunc
	userAgent string

	mu      sync.Mutex
	cached  string
	fetched time.Time
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client, for example one from client-go's
// rest.HTTPClientFor that already authenticates requests.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.http = c }
}

// WithToken authenticates requests with a bearer token from fn. Tokens are
// reused for up to a minute, and refreshed once when the API answers 401.
// Datumctl plugins pass plugin.Token.
func WithToken(fn TokenFunc) Option {
	return func(cl *Client) { cl.token = fn }
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(cl *Client) { cl.userAgent = ua }
}

// New returns a client for the logs API aggregated under controlPlane, a
// control-plane URL such as ProjectURL returns.
func New(controlPlane string, opts ...Option) *Client {
	c := &Client{
		base: strings.TrimRight(controlPlane, "/") + APIPath,
		http: &http.Client{Timeout: requestTimeout},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ProjectURL is the control-plane URL of project on apiHost. apiHost may omit
// its scheme, as datumctl's DATUM_API_HOST does; https is assumed.
func ProjectURL(apiHost, project string) string {
	if !strings.Contains(apiHost, "://") {
		apiHost = "https://" + apiHost
	}
	return strings.TrimRight(apiHost, "/") +
		"/apis/resourcemanager.miloapis.com/v1alpha1/projects/" + url.PathEscape(project) + "/control-plane"
}

// QueryRange returns the lines matching q, merged across streams and ordered
// by q.Direction. The API groups lines by stream, so without the merge the
// clock would run backwards at every stream boundary.
func (c *Client) QueryRange(ctx context.Context, q Query) ([]Entry, error) {
	v := between(q.Start, q.End)
	v.Set("query", q.Query)
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Direction != "" {
		v.Set("direction", string(q.Direction))
	}
	var data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"result"`
	}
	if err := c.get(ctx, "/query_range", v, &data); err != nil {
		return nil, err
	}
	if data.ResultType != "streams" {
		return nil, fmt.Errorf("logs query returned %q results, expected streams", data.ResultType)
	}

	var entries []Entry
	for _, s := range data.Result {
		key := canonical(s.Stream)
		for _, val := range s.Values {
			ns, err := strconv.ParseInt(val[0], 10, 64)
			if err != nil {
				continue
			}
			entries = append(entries, Entry{Time: time.Unix(0, ns), Labels: s.Stream, Line: val[1], stream: key})
		}
	}
	Sort(entries, q.Direction)
	return entries, nil
}

// LabelNames returns the label names seen on lines in [start, end).
func (c *Client) LabelNames(ctx context.Context, start, end time.Time) ([]string, error) {
	var names []string
	err := c.get(ctx, "/labels", between(start, end), &names)
	return names, err
}

// LabelValues returns the values label takes on lines in [start, end).
func (c *Client) LabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error) {
	var values []string
	err := c.get(ctx, "/label/"+url.PathEscape(label)+"/values", between(start, end), &values)
	return values, err
}

// Sort orders entries by timestamp in dir, Backward when dir is empty. It is
// stable, so lines sharing a timestamp keep their order.
func Sort(entries []Entry, dir Direction) {
	slices.SortStableFunc(entries, func(a, b Entry) int {
		if dir == Forward {
			return a.Time.Compare(b.Time)
		}
		return b.Time.Compare(a.Time)
	})
}

// get fetches path into data, retrying once with a fresh token on 401.
func (c *Client) get(ctx context.Context, path string, q url.Values, data any) error {
	u := c.base + path + "?" + q.Encode()
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if c.userAgent != "" {
			req.Header.Set("User-Agent", c.userAgent)
		}
		if c.token != nil {
			token, err := c.bearer(attempt > 0)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("querying logs: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.token != nil {
			_ = resp.Body.Close()
			continue
		}
		return decode(resp, data)
	}
}

func (c *Client) bearer(refresh bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if refresh || c.cached == "" || time.Since(c.fetched) > tokenTTL {
		t, err := c.token()
		if err != nil {
			return "", fmt.Errorf("getting credentials: %w", err)
		}
		c.cached, c.fetched = t, time.Now()
	}
	return c.cached, nil
}

func decode(resp *http.Response, data any) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	var envelope struct {
		Status string          `json:"status"`
		Error  string          `json:"error"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decoding logs response: %w", err)
	}
	if envelope.Status != "success" {
		return fmt.Errorf("logs query failed: %s", cmp.Or(envelope.Error, "status "+envelope.Status))
	}
	if err := json.Unmarshal(envelope.Data, data); err != nil {
		return fmt.Errorf("decoding logs response: %w", err)
	}
	return nil
}

// APIError is a non-200 answer from the logs API.
type APIError struct {
	StatusCode int
	Status     string // e.g. "403 Forbidden"
	// Detail is the server's reason: a query parse error from the API's own
	// Loki envelope, or a denial from the aggregator's Kubernetes Status.
	// Without it a rejected query and a permission denial look the same.
	Detail string
}

func (e *APIError) Error() string {
	msg := "logs query returned " + e.Status
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Hint suggests what a datumctl user can do about the error, or "" if nothing
// specific applies.
func (e *APIError) Hint() string {
	switch e.StatusCode {
	case http.StatusBadRequest:
		return `queries accept label matchers and line filters only, e.g. {service_name="checkout"} |= "error"`
	case http.StatusUnauthorized:
		return "run 'datumctl login' to refresh your credentials"
	case http.StatusForbidden:
		return "reading logs requires the o11y.miloapis.com logs.query permission on this project"
	case http.StatusNotFound:
		return "this project may not have the telemetry API enabled"
	}
	return ""
}

// Permanent reports whether retrying err cannot help: the query is malformed,
// the caller may not read logs, or the project does not serve them.
func Permanent(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

func responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	detail := strings.TrimSpace(string(body))

	// Loki envelopes carry "error"; Kubernetes Status objects carry "message".
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		detail = cmp.Or(envelope.Error, envelope.Message, detail)
	}
	return &APIError{StatusCode: resp.StatusCode, Status: resp.Status, Detail: detail}
}

// between is the window parameters in nanoseconds. The API currently truncates
// both bounds to whole seconds anyway; see Pages.
func between(start, end time.Time) url.Values {
	v := url.Values{}
	if !start.IsZero() {
		v.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	}
	if !end.IsZero() {
		v.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	}
	return v
}

func canonical(labels map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		b.WriteString(k + "=" + labels[k] + "\x01")
	}
	return b.String()
}
