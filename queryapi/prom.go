// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.datum.net/o11y/queryapi/internal/storage"
)

// maxPoints is Prometheus's own ceiling on samples per series in a range
// query.
const maxPoints = 11000

// promForm parses the URL query and, for POST, the form body.
func promForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	if err := r.ParseForm(); err != nil {
		writePromError(w, http.StatusBadRequest, "bad_data", "invalid form: "+err.Error())
		return nil, false
	}
	return r.Form, true
}

func required(w http.ResponseWriter, form url.Values, name string) (string, bool) {
	v := strings.TrimSpace(form.Get(name))
	if v == "" {
		writePromError(w, http.StatusBadRequest, "bad_data", name+" is required")
		return "", false
	}
	return v, true
}

func promTime(w http.ResponseWriter, form url.Values, name string, fallback time.Time) (time.Time, bool) {
	t, err := parsePromTime(form.Get(name), fallback)
	if err != nil {
		writePromError(w, http.StatusBadRequest, "bad_data", name+": "+err.Error())
		return time.Time{}, false
	}
	return t, true
}

// promTimeout caps a client timeout at --query-timeout.
func (h *handler) promTimeout(w http.ResponseWriter, form url.Values) (time.Duration, bool) {
	raw := form.Get("timeout")
	if raw == "" {
		return h.cfg.QueryTimeout, true
	}
	d, err := parsePromDuration(raw)
	if err != nil {
		writePromError(w, http.StatusBadRequest, "bad_data", "timeout: "+err.Error())
		return 0, false
	}
	return min(d, h.cfg.QueryTimeout), true
}

// promRange reads optional start/end. Absent bounds stay zero, which the
// store leaves to the backend's default.
func promRange(w http.ResponseWriter, form url.Values) (storage.TimeRange, bool) {
	start, ok := promTime(w, form, "start", time.Time{})
	if !ok {
		return storage.TimeRange{}, false
	}
	end, ok := promTime(w, form, "end", time.Time{})
	if !ok {
		return storage.TimeRange{}, false
	}
	if !start.IsZero() && !end.IsZero() && end.Before(start) {
		writePromError(w, http.StatusBadRequest, "bad_data", "end must not be before start")
		return storage.TimeRange{}, false
	}
	return storage.TimeRange{Start: start, End: end}, true
}

func (h *handler) promQuery(w http.ResponseWriter, r *http.Request) {
	form, ok := promForm(w, r)
	if !ok {
		return
	}
	query, ok := required(w, form, "query")
	if !ok {
		return
	}
	at, ok := promTime(w, form, "time", time.Now().UTC())
	if !ok {
		return
	}
	timeout, ok := h.promTimeout(w, form)
	if !ok {
		return
	}
	ctx, cancel := h.withTimeout(r)
	defer cancel()
	data, err := h.metrics.Query(ctx, storage.InstantQuery{Query: query, Time: at, Timeout: timeout})
	h.writePromResult(w, data, err)
}

func (h *handler) promQueryRange(w http.ResponseWriter, r *http.Request) {
	form, ok := promForm(w, r)
	if !ok {
		return
	}
	query, ok := required(w, form, "query")
	if !ok {
		return
	}
	for _, name := range []string{"start", "end", "step"} {
		if _, ok := required(w, form, name); !ok {
			return
		}
	}
	tr, ok := promRange(w, form)
	if !ok {
		return
	}
	step, err := parsePromDuration(form.Get("step"))
	if err != nil {
		writePromError(w, http.StatusBadRequest, "bad_data", "step: "+err.Error())
		return
	}
	window := tr.End.Sub(tr.Start)
	if window > h.cfg.MaxRange {
		writePromError(w, http.StatusBadRequest, "bad_data",
			"range "+window.String()+" exceeds the maximum of "+h.cfg.MaxRange.String())
		return
	}
	if int64(window/step)+1 > maxPoints {
		writePromError(w, http.StatusBadRequest, "bad_data",
			"exceeded maximum resolution of 11,000 points per timeseries; try a larger step")
		return
	}
	timeout, ok := h.promTimeout(w, form)
	if !ok {
		return
	}
	ctx, cancel := h.withTimeout(r)
	defer cancel()
	data, err := h.metrics.QueryRange(ctx, storage.RangeQuery{Query: query, Range: tr, Step: step, Timeout: timeout})
	h.writePromResult(w, data, err)
}

func (h *handler) promSeries(w http.ResponseWriter, r *http.Request) {
	form, ok := promForm(w, r)
	if !ok {
		return
	}
	matchers := form["match[]"]
	if len(matchers) == 0 {
		writePromError(w, http.StatusBadRequest, "bad_data", "at least one match[] selector is required")
		return
	}
	tr, ok := promRange(w, form)
	if !ok {
		return
	}
	ctx, cancel := h.withTimeout(r)
	defer cancel()
	data, err := h.metrics.Series(ctx, matchers, tr)
	h.writePromResult(w, data, err)
}

func (h *handler) promLabelNames(w http.ResponseWriter, r *http.Request) {
	form, ok := promForm(w, r)
	if !ok {
		return
	}
	tr, ok := promRange(w, form)
	if !ok {
		return
	}
	ctx, cancel := h.withTimeout(r)
	defer cancel()
	data, err := h.metrics.LabelNames(ctx, form["match[]"], tr)
	h.writePromResult(w, data, err)
}

func (h *handler) promLabelValues(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !storage.ValidLabelName(name) {
		writePromError(w, http.StatusBadRequest, "bad_data", "invalid label name "+strconv.Quote(name))
		return
	}
	form, ok := promForm(w, r)
	if !ok {
		return
	}
	tr, ok := promRange(w, form)
	if !ok {
		return
	}
	ctx, cancel := h.withTimeout(r)
	defer cancel()
	data, err := h.metrics.LabelValues(ctx, name, form["match[]"], tr)
	h.writePromResult(w, data, err)
}

func (h *handler) writePromResult(w http.ResponseWriter, data json.RawMessage, err error) {
	if err != nil {
		h.writePromStorageError(w, err)
		return
	}
	writeJSON(w, struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}{Status: "success", Data: data})
}

// writePromStorageError maps metric store failures. Only a QueryError's text
// reaches the client; anything about the backend is logged.
func (h *handler) writePromStorageError(w http.ResponseWriter, err error) {
	var qe *storage.QueryError
	switch {
	case errors.As(err, &qe):
		writePromError(w, http.StatusBadRequest, "bad_data", qe.Message)
	case errors.Is(err, storage.ErrNoProject):
		writePromError(w, http.StatusUnauthorized, "unauthorized", "request is not scoped to a project")
	case errors.Is(err, storage.ErrResponseTooLarge):
		writePromError(w, http.StatusUnprocessableEntity, "execution",
			"response exceeds the size limit; narrow the query or the time range")
	case errors.Is(err, context.Canceled):
		writePromError(w, 499, "canceled", "client cancelled request")
	case errors.Is(err, context.DeadlineExceeded):
		writePromError(w, http.StatusServiceUnavailable, "timeout", "query timed out")
	case errors.Is(err, storage.ErrBackendUnavailable):
		h.logger.Error("metrics backend unavailable", "error", err)
		writePromError(w, http.StatusBadGateway, "unavailable", "metrics backend unavailable")
	default:
		h.logger.Error("metric query failed", "error", err)
		writePromError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// writePromError writes Prometheus's error envelope.
func writePromError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}{Status: "error", ErrorType: errorType, Error: message})
}
