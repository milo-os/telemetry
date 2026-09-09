// SPDX-License-Identifier: AGPL-3.0-only

// Package fake generates synthetic logs so the query API is useful before
// ClickHouse ingest is healthy. Rows are a pure function of their timestamp
// and the configured rate, so results are identical across restarts and
// across replicas sharing one rate.
package fake

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"time"

	"go.datum.net/o11y/queryapi/internal/logql"
	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
)

type service struct {
	name      string
	weight    int
	resources []string
}

var services = []service{
	{"envoy-gateway", 55, []string{"gateway-us-east", "gateway-eu-west"}},
	{"waf", 20, []string{"gateway-us-east", "gateway-eu-west"}},
	{"compute-workload", 25, []string{"checkout-api", "payments-api"}},
}

type severity struct {
	name   string
	weight int
}

var severities = []severity{{"INFO", 78}, {"WARN", 12}, {"ERROR", 8}, {"DEBUG", 2}}

var (
	methods    = []string{"GET", "POST", "PUT", "DELETE"}
	paths      = []string{"/api/v1/checkout", "/api/v1/products", "/healthz", "/api/v1/payments"}
	statuses   = []int{200, 201, 301, 400, 404, 429, 500, 503}
	wafRules   = []string{"rule-942100-sqli", "rule-941100-xss", "rule-930100-lfi"}
	wafActions = []string{"blocked", "logged"}
	appEvents  = []string{
		"order placed", "cache miss", "payment authorised",
		"upstream timeout", "retrying request", "worker started",
	}
)

// Store is a synthetic storage.LogStore.
type Store struct {
	interval time.Duration
}

// New returns a Store generating ratePerSecond lines per second.
func New(ratePerSecond float64) *Store {
	if ratePerSecond <= 0 {
		ratePerSecond = 1
	}
	return &Store{interval: time.Duration(float64(time.Second) / ratePerSecond)}
}

func (s *Store) QueryLogs(ctx context.Context, q storage.LogQuery) (storage.LogIterator, error) {
	if _, ok := miloauth.ProjectID(ctx); !ok {
		return nil, storage.ErrNoProject
	}
	if q.Limit <= 0 {
		return nil, storage.ErrInvalidLimit
	}
	return newIterator(ctx, s.interval, q), nil
}

func (s *Store) LabelNames(ctx context.Context, _ storage.TimeRange) ([]string, error) {
	if _, ok := miloauth.ProjectID(ctx); !ok {
		return nil, storage.ErrNoProject
	}
	return []string{"resource_name", "service_name", "severity"}, nil
}

func (s *Store) LabelValues(ctx context.Context, label string, _ storage.TimeRange) ([]string, error) {
	if _, ok := miloauth.ProjectID(ctx); !ok {
		return nil, storage.ErrNoProject
	}

	seen := map[string]bool{}
	for _, ls := range catalogue() {
		if v, ok := ls[label]; ok {
			seen[v] = true
		}
	}
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	sort.Strings(values)
	return values, nil
}

func (s *Store) Series(ctx context.Context, matchers []logql.LabelMatcher, _ storage.TimeRange) ([]storage.LabelSet, error) {
	if _, ok := miloauth.ProjectID(ctx); !ok {
		return nil, storage.ErrNoProject
	}

	var out []storage.LabelSet
	for _, ls := range catalogue() {
		if matchesLabels(matchers, ls) {
			out = append(out, ls)
		}
	}
	return out, nil
}

func (s *Store) Ping(context.Context) error { return nil }

// catalogue is every label set the generator can produce. Discovery is served
// from here rather than from generated rows, so it is instant and populates
// Grafana's stream-selector UI even for empty windows.
func catalogue() []storage.LabelSet {
	var out []storage.LabelSet
	for _, svc := range services {
		for _, sev := range severities {
			for _, res := range svc.resources {
				out = append(out, storage.LabelSet{
					"service_name":  svc.name,
					"severity":      sev.name,
					"resource_name": res,
				})
			}
		}
	}
	return out
}

func matchesLabels(matchers []logql.LabelMatcher, labels storage.LabelSet) bool {
	for _, m := range matchers {
		if !m.Matches(labels[m.Label]) {
			return false
		}
	}
	return true
}

// rowAt synthesises the row for tick index i. Seeding from i alone is what
// makes the output reproducible.
func rowAt(i int64, interval time.Duration) storage.Row {
	h := fnv.New64a()
	var buf [8]byte
	for b := 0; b < 8; b++ {
		buf[b] = byte(i >> (8 * b))
	}
	_, _ = h.Write(buf[:])
	rnd := rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec // synthetic data, not security

	svc := pickService(rnd)
	sev := pickSeverity(rnd)
	res := svc.resources[rnd.Intn(len(svc.resources))]

	return storage.Row{
		Timestamp: time.Unix(0, i*int64(interval)).UTC(),
		Labels: storage.LabelSet{
			"service_name":  svc.name,
			"severity":      sev,
			"resource_name": res,
		},
		Line: body(rnd, svc.name, sev, res),
	}
}

func pickService(rnd *rand.Rand) service {
	total := 0
	for _, s := range services {
		total += s.weight
	}
	n := rnd.Intn(total)
	for _, s := range services {
		if n < s.weight {
			return s
		}
		n -= s.weight
	}
	return services[len(services)-1]
}

func pickSeverity(rnd *rand.Rand) string {
	total := 0
	for _, s := range severities {
		total += s.weight
	}
	n := rnd.Intn(total)
	for _, s := range severities {
		if n < s.weight {
			return s.name
		}
		n -= s.weight
	}
	return severities[len(severities)-1].name
}

func body(rnd *rand.Rand, svc, sev, res string) string {
	switch svc {
	case "envoy-gateway":
		return fmt.Sprintf("%s %s %d %dms upstream=%s",
			methods[rnd.Intn(len(methods))],
			paths[rnd.Intn(len(paths))],
			statuses[rnd.Intn(len(statuses))],
			rnd.Intn(2000),
			res)
	case "waf":
		return fmt.Sprintf("%s request matched %s client=%d.%d.%d.%d gateway=%s",
			wafActions[rnd.Intn(len(wafActions))],
			wafRules[rnd.Intn(len(wafRules))],
			rnd.Intn(256), rnd.Intn(256), rnd.Intn(256), rnd.Intn(256),
			res)
	default:
		return fmt.Sprintf("%s: %s service=%s", sev, appEvents[rnd.Intn(len(appEvents))], res)
	}
}
