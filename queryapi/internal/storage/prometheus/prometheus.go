// SPDX-License-Identifier: AGPL-3.0-only

// Package prometheus is a storage.MetricStore for any server implementing the
// Prometheus query HTTP API. Tenancy is a separate, explicit choice: see
// Tenancy.
package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// Tenancy is how the backend is made to confine a query to one project.
type Tenancy string

// TenancyExtraLabel sends VictoriaMetrics's extra_label parameter. Backends
// that don't implement it, the Prometheus server among them, ignore it and
// apply no isolation, so this mode requires VictoriaMetrics or a server that
// honors extra_label.
const TenancyExtraLabel Tenancy = "extra-label"

// ParseTenancy validates a --prometheus-tenancy value.
func ParseTenancy(s string) (Tenancy, error) {
	switch Tenancy(s) {
	case TenancyExtraLabel:
		return TenancyExtraLabel, nil
	case "":
		return "", errors.New("prometheus tenancy is required; the only mode is extra-label")
	default:
		return "", fmt.Errorf("unknown prometheus tenancy %q; the only mode is extra-label", s)
	}
}

// Config configures a Store.
type Config struct {
	URL              string
	Tenancy          Tenancy
	ProjectLabel     string
	MaxResponseBytes int64
	Client           *http.Client
}

// Store is a storage.MetricStore over the Prometheus query API.
type Store struct {
	base   *url.URL
	cfg    Config
	client *http.Client
}

var _ storage.MetricStore = (*Store)(nil)

// New validates cfg and returns a Store.
func New(cfg Config) (*Store, error) {
	if _, err := ParseTenancy(string(cfg.Tenancy)); err != nil {
		return nil, err
	}
	base, err := url.Parse(cfg.URL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("prometheus url %q must be an absolute http or https URL", cfg.URL)
	}
	if !storage.ValidLabelName(cfg.ProjectLabel) {
		return nil, fmt.Errorf("prometheus project label %q is not a valid label name", cfg.ProjectLabel)
	}
	if cfg.MaxResponseBytes <= 0 {
		return nil, fmt.Errorf("max response bytes must be greater than zero, got %d", cfg.MaxResponseBytes)
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	return &Store{base: base, cfg: cfg, client: client}, nil
}

func (s *Store) Query(ctx context.Context, q storage.InstantQuery) (json.RawMessage, error) {
	form := url.Values{"query": {q.Query}, "time": {formatTime(q.Time)}}
	if q.Timeout > 0 {
		form.Set("timeout", formatSeconds(q.Timeout))
	}
	return s.do(ctx, "/api/v1/query", "/api/v1/query", form)
}

func (s *Store) QueryRange(ctx context.Context, q storage.RangeQuery) (json.RawMessage, error) {
	form := url.Values{
		"query": {q.Query},
		"start": {formatTime(q.Range.Start)},
		"end":   {formatTime(q.Range.End)},
		"step":  {formatSeconds(q.Step)},
	}
	if q.Timeout > 0 {
		form.Set("timeout", formatSeconds(q.Timeout))
	}
	return s.do(ctx, "/api/v1/query_range", "/api/v1/query_range", form)
}

func (s *Store) Series(ctx context.Context, matchers []string, tr storage.TimeRange) (json.RawMessage, error) {
	return s.do(ctx, "/api/v1/series", "/api/v1/series", selectorForm(matchers, tr))
}

func (s *Store) LabelNames(ctx context.Context, matchers []string, tr storage.TimeRange) (json.RawMessage, error) {
	return s.do(ctx, "/api/v1/labels", "/api/v1/labels", selectorForm(matchers, tr))
}

func (s *Store) LabelValues(ctx context.Context, name string, matchers []string, tr storage.TimeRange) (json.RawMessage, error) {
	if !storage.ValidLabelName(name) {
		return nil, &storage.QueryError{Message: "invalid label name " + strconv.Quote(name)}
	}
	// The metric label is the route, never the name, so it stays bounded.
	return s.do(ctx, "/api/v1/label/"+name+"/values", "/api/v1/label/{name}/values", selectorForm(matchers, tr))
}

// Ping checks /-/healthy, which both Prometheus and VictoriaMetrics serve.
func (s *Store) Ping(ctx context.Context) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base.JoinPath("/-/healthy").String(), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", storage.ErrBackendUnavailable, err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: /-/healthy returned %d", storage.ErrBackendUnavailable, resp.StatusCode)
	}
	return nil
}

func selectorForm(matchers []string, tr storage.TimeRange) url.Values {
	form := url.Values{}
	for _, m := range matchers {
		form.Add("match[]", m)
	}
	if !tr.Start.IsZero() {
		form.Set("start", formatTime(tr.Start))
	}
	if !tr.End.IsZero() {
		form.Set("end", formatTime(tr.End))
	}
	return form
}

// do sends form to path, confined to the project on ctx. route is the metric
// label for path.
func (s *Store) do(ctx context.Context, path, route string, form url.Values) (data json.RawMessage, err error) {
	project, ok := miloauth.ProjectID(ctx)
	if !ok {
		return nil, storage.ErrNoProject
	}
	// Set, not Add: form is built from typed fields, so this is the only
	// extra_label it can carry.
	form.Set("extra_label", s.cfg.ProjectLabel+"="+project)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base.JoinPath(path).String(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	start := time.Now()
	resp, err := s.client.Do(req)
	backendDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	if err != nil {
		backendRequests.WithLabelValues(route, "error").Inc()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %w", storage.ErrBackendUnavailable, err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	backendRequests.WithLabelValues(route, strconv.Itoa(resp.StatusCode)).Inc()

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxResponseBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: read response: %w", storage.ErrBackendUnavailable, err)
	}
	if int64(len(body)) > s.cfg.MaxResponseBytes {
		return nil, storage.ErrResponseTooLarge
	}
	data, err = decode(resp.StatusCode, body)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	return data, err
}

type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

// decode trusts a body only if it is a Prometheus API envelope. Anything else,
// such as a proxy's HTML, means the backend is unavailable.
func decode(status int, body []byte) (json.RawMessage, error) {
	var env envelope
	isEnvelope := json.Unmarshal(body, &env) == nil && env.Status != ""
	switch {
	case isEnvelope && (status == http.StatusUnprocessableEntity || status == http.StatusServiceUnavailable) && isTimeout(env):
		return nil, fmt.Errorf("%w: %s", context.DeadlineExceeded, env.Error)
	case status == http.StatusOK && isEnvelope && env.Status == "success" && len(env.Data) > 0:
		return env.Data, nil
	case (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) && isEnvelope && env.Error != "":
		return nil, &storage.QueryError{Message: env.Error}
	default:
		return nil, fmt.Errorf("%w: status %d: %.512s", storage.ErrBackendUnavailable, status, body)
	}
}

// isTimeout recognises a backend query timeout. Prometheus sends errorType
// "timeout"; VictoriaMetrics sends the HTTP status as errorType (observed
// 422) and says "timeout exceeded during query execution".
func isTimeout(env envelope) bool {
	return env.ErrorType == "timeout" || strings.Contains(env.Error, "timeout exceeded")
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func formatSeconds(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) }
