package unbatchprocessor // import "go.datum.net/o11y/processor/unbatchprocessor"

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

type unbatchLogs struct {
	next consumer.Logs
}

func newUnbatchLogs(next consumer.Logs) *unbatchLogs { return &unbatchLogs{next: next} }

func (p *unbatchLogs) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}
func (p *unbatchLogs) Start(context.Context, component.Host) error { return nil }
func (p *unbatchLogs) Shutdown(context.Context) error              { return nil }

func (p *unbatchLogs) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	for _, single := range splitLogs(ld) {
		if err := p.next.ConsumeLogs(ctx, single); err != nil {
			return err
		}
	}
	return nil
}

// splitLogs returns one plog.Logs per log record, each carrying a copy of its
// parent ResourceLogs' Resource and ScopeLogs' Scope. A ld with at most one
// record is returned unchanged (as its own one-element slice), so unbatch is a
// no-op when chained after itself or another unbatch-shaped processor; a
// zero-record ld produces no output.
func splitLogs(ld plog.Logs) []plog.Logs {
	switch ld.LogRecordCount() {
	case 0:
		return nil
	case 1:
		return []plog.Logs{ld}
	}

	result := make([]plog.Logs, 0, ld.LogRecordCount())
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		rl := ld.ResourceLogs().At(i)
		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			sl := rl.ScopeLogs().At(j)
			for k := 0; k < sl.LogRecords().Len(); k++ {
				single := plog.NewLogs()
				newRL := single.ResourceLogs().AppendEmpty()
				rl.Resource().CopyTo(newRL.Resource())
				newRL.SetSchemaUrl(rl.SchemaUrl())
				newSL := newRL.ScopeLogs().AppendEmpty()
				sl.Scope().CopyTo(newSL.Scope())
				newSL.SetSchemaUrl(sl.SchemaUrl())
				sl.LogRecords().At(k).CopyTo(newSL.LogRecords().AppendEmpty())
				result = append(result, single)
			}
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Traces
// ---------------------------------------------------------------------------

type unbatchTraces struct {
	next consumer.Traces
}

func newUnbatchTraces(next consumer.Traces) *unbatchTraces { return &unbatchTraces{next: next} }

func (p *unbatchTraces) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}
func (p *unbatchTraces) Start(context.Context, component.Host) error { return nil }
func (p *unbatchTraces) Shutdown(context.Context) error              { return nil }

func (p *unbatchTraces) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	for _, single := range splitTraces(td) {
		if err := p.next.ConsumeTraces(ctx, single); err != nil {
			return err
		}
	}
	return nil
}

// splitTraces mirrors splitLogs, one span per output ptrace.Traces.
func splitTraces(td ptrace.Traces) []ptrace.Traces {
	switch td.SpanCount() {
	case 0:
		return nil
	case 1:
		return []ptrace.Traces{td}
	}

	result := make([]ptrace.Traces, 0, td.SpanCount())
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			for k := 0; k < ss.Spans().Len(); k++ {
				single := ptrace.NewTraces()
				newRS := single.ResourceSpans().AppendEmpty()
				rs.Resource().CopyTo(newRS.Resource())
				newRS.SetSchemaUrl(rs.SchemaUrl())
				newSS := newRS.ScopeSpans().AppendEmpty()
				ss.Scope().CopyTo(newSS.Scope())
				newSS.SetSchemaUrl(ss.SchemaUrl())
				ss.Spans().At(k).CopyTo(newSS.Spans().AppendEmpty())
				result = append(result, single)
			}
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

type unbatchMetrics struct {
	next consumer.Metrics
}

func newUnbatchMetrics(next consumer.Metrics) *unbatchMetrics { return &unbatchMetrics{next: next} }

func (p *unbatchMetrics) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}
func (p *unbatchMetrics) Start(context.Context, component.Host) error { return nil }
func (p *unbatchMetrics) Shutdown(context.Context) error              { return nil }

func (p *unbatchMetrics) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	for _, single := range splitMetrics(md) {
		if err := p.next.ConsumeMetrics(ctx, single); err != nil {
			return err
		}
	}
	return nil
}

// splitMetrics returns one pmetric.Metrics per data point in md. Unlike
// logs/traces, metrics have no single "record" type -- each metric type
// (Gauge, Sum, Histogram, ExponentialHistogram, Summary) carries its own
// DataPoints, so the metric-level metadata (name, description, unit, and
// type-specific fields like aggregation temporality) is copied onto each
// single-data-point output.
func splitMetrics(md pmetric.Metrics) []pmetric.Metrics {
	switch md.DataPointCount() {
	case 0:
		return nil
	case 1:
		return []pmetric.Metrics{md}
	}

	result := make([]pmetric.Metrics, 0, md.DataPointCount())
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			for k := 0; k < sm.Metrics().Len(); k++ {
				result = append(result, explodeMetric(rm, sm, sm.Metrics().At(k))...)
			}
		}
	}
	return result
}

// newMetricContainer builds a single ResourceMetrics/ScopeMetrics/Metric
// shell (Resource, Scope, and the source metric's name/description/unit
// copied over), ready for exactly one data point to be added to it.
func newMetricContainer(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric) (pmetric.Metrics, pmetric.Metric) {
	single := pmetric.NewMetrics()
	newRM := single.ResourceMetrics().AppendEmpty()
	rm.Resource().CopyTo(newRM.Resource())
	newRM.SetSchemaUrl(rm.SchemaUrl())
	newSM := newRM.ScopeMetrics().AppendEmpty()
	sm.Scope().CopyTo(newSM.Scope())
	newSM.SetSchemaUrl(sm.SchemaUrl())
	newM := newSM.Metrics().AppendEmpty()
	newM.SetName(m.Name())
	newM.SetDescription(m.Description())
	newM.SetUnit(m.Unit())
	return single, newM
}

func explodeMetric(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric) []pmetric.Metrics {
	var result []pmetric.Metrics

	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			single, newM := newMetricContainer(rm, sm, m)
			newM.SetEmptyGauge()
			dps.At(i).CopyTo(newM.Gauge().DataPoints().AppendEmpty())
			result = append(result, single)
		}
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			single, newM := newMetricContainer(rm, sm, m)
			newM.SetEmptySum()
			newM.Sum().SetAggregationTemporality(m.Sum().AggregationTemporality())
			newM.Sum().SetIsMonotonic(m.Sum().IsMonotonic())
			dps.At(i).CopyTo(newM.Sum().DataPoints().AppendEmpty())
			result = append(result, single)
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			single, newM := newMetricContainer(rm, sm, m)
			newM.SetEmptyHistogram()
			newM.Histogram().SetAggregationTemporality(m.Histogram().AggregationTemporality())
			dps.At(i).CopyTo(newM.Histogram().DataPoints().AppendEmpty())
			result = append(result, single)
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			single, newM := newMetricContainer(rm, sm, m)
			newM.SetEmptyExponentialHistogram()
			newM.ExponentialHistogram().SetAggregationTemporality(m.ExponentialHistogram().AggregationTemporality())
			dps.At(i).CopyTo(newM.ExponentialHistogram().DataPoints().AppendEmpty())
			result = append(result, single)
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			single, newM := newMetricContainer(rm, sm, m)
			newM.SetEmptySummary()
			dps.At(i).CopyTo(newM.Summary().DataPoints().AppendEmpty())
			result = append(result, single)
		}
	}

	return result
}
