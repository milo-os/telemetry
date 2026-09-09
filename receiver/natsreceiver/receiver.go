package natsreceiver // import "go.datum.net/o11y/receiver/natsreceiver"

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
)

// transport labels the otelcol_receiver_* series this component emits.
const transport = "nats"

// newObsReport builds the reporter behind otelcol_receiver_accepted_* and
// otelcol_receiver_refused_*. Every receiver has to emit these itself; without
// it the component is invisible to the collector's standard dashboards.
func newObsReport(set receiver.Settings) (*receiverhelper.ObsReport, error) {
	return receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             set.ID,
		Transport:              transport,
		ReceiverCreateSettings: set,
	})
}

// nakRedeliveryDelay is passed to NakWithDelay for retryable (e.g. downstream
// export) failures. A bare Nak would spin a message in a tight, backoff-free
// loop while a transient condition (e.g. ClickHouse briefly unavailable) clears.
const nakRedeliveryDelay = 2 * time.Second

// errUnmarshal wraps a payload-unmarshal failure. Redelivering the same
// malformed payload can never succeed, so handleJetStream treats it as
// non-retryable: it acks (drops) the message instead of nak'ing it into an
// infinite redelivery loop.
type errUnmarshal struct {
	err error
}

func (e *errUnmarshal) Error() string { return e.err.Error() }
func (e *errUnmarshal) Unwrap() error { return e.err }

type natsReceiver struct {
	cfg    *Config
	set    receiver.Settings
	signal SignalConfig

	// deliverPayload unmarshals payload into the signal-specific pdata type
	// and forwards it to this instance's consumer. Wraps an unmarshal failure
	// in &errUnmarshal{} (non-retryable) to distinguish it from a downstream
	// forwarding failure (retryable).
	deliverPayload func(ctx context.Context, payload []byte) error

	conn        *nats.Conn
	consumeCtx  jetstream.ConsumeContext
	coreNATSSub *nats.Subscription

	// lastDelivery holds unix-nanosecond timestamps of the last successful
	// delivery. Written from NATS delivery goroutines, read on scrape; atomic
	// avoids a mutex for that cross-goroutine access.
	lastDelivery      atomic.Int64
	lastDeliveryGauge metric.Float64ObservableGauge
	resubscribeCount  metric.Int64Counter
}

func newLogsReceiver(cfg *Config, set receiver.Settings, next consumer.Logs) (*natsReceiver, error) {
	unmarshaler, err := newLogsUnmarshaler(cfg.Logs.Encoding)
	if err != nil {
		return nil, err
	}
	obsrecv, err := newObsReport(set)
	if err != nil {
		return nil, err
	}
	encoding := cfg.Logs.Encoding
	return &natsReceiver{
		cfg:    cfg,
		set:    set,
		signal: cfg.Logs,
		deliverPayload: func(ctx context.Context, payload []byte) error {
			ctx = obsrecv.StartLogsOp(ctx)
			logs, err := unmarshaler.UnmarshalLogs(payload)
			if err != nil {
				obsrecv.EndLogsOp(ctx, encoding, 0, err)
				return &errUnmarshal{err: err}
			}
			// Counted before Consume: pdata is not safe to read once the next
			// consumer owns it.
			count := logs.LogRecordCount()
			err = next.ConsumeLogs(ctx, logs)
			obsrecv.EndLogsOp(ctx, encoding, count, err)
			return err
		},
	}, nil
}

func newMetricsReceiver(cfg *Config, set receiver.Settings, next consumer.Metrics) (*natsReceiver, error) {
	unmarshaler, err := newMetricsUnmarshaler(cfg.Metrics.Encoding)
	if err != nil {
		return nil, err
	}
	obsrecv, err := newObsReport(set)
	if err != nil {
		return nil, err
	}
	encoding := cfg.Metrics.Encoding
	return &natsReceiver{
		cfg:    cfg,
		set:    set,
		signal: cfg.Metrics,
		deliverPayload: func(ctx context.Context, payload []byte) error {
			ctx = obsrecv.StartMetricsOp(ctx)
			metrics, err := unmarshaler.UnmarshalMetrics(payload)
			if err != nil {
				obsrecv.EndMetricsOp(ctx, encoding, 0, err)
				return &errUnmarshal{err: err}
			}
			count := metrics.DataPointCount()
			err = next.ConsumeMetrics(ctx, metrics)
			obsrecv.EndMetricsOp(ctx, encoding, count, err)
			return err
		},
	}, nil
}

func newTracesReceiver(cfg *Config, set receiver.Settings, next consumer.Traces) (*natsReceiver, error) {
	unmarshaler, err := newTracesUnmarshaler(cfg.Traces.Encoding)
	if err != nil {
		return nil, err
	}
	obsrecv, err := newObsReport(set)
	if err != nil {
		return nil, err
	}
	encoding := cfg.Traces.Encoding
	return &natsReceiver{
		cfg:    cfg,
		set:    set,
		signal: cfg.Traces,
		deliverPayload: func(ctx context.Context, payload []byte) error {
			ctx = obsrecv.StartTracesOp(ctx)
			traces, err := unmarshaler.UnmarshalTraces(payload)
			if err != nil {
				obsrecv.EndTracesOp(ctx, encoding, 0, err)
				return &errUnmarshal{err: err}
			}
			count := traces.SpanCount()
			err = next.ConsumeTraces(ctx, traces)
			obsrecv.EndTracesOp(ctx, encoding, count, err)
			return err
		},
	}, nil
}

func newLogsUnmarshaler(encoding string) (plog.Unmarshaler, error) {
	switch encoding {
	case encodingOTLPProto:
		return &plog.ProtoUnmarshaler{}, nil
	case encodingOTLPJSON:
		return &plog.JSONUnmarshaler{}, nil
	default:
		return nil, fmt.Errorf("unsupported logs encoding %q", encoding)
	}
}

func newMetricsUnmarshaler(encoding string) (pmetric.Unmarshaler, error) {
	switch encoding {
	case encodingOTLPProto:
		return &pmetric.ProtoUnmarshaler{}, nil
	case encodingOTLPJSON:
		return &pmetric.JSONUnmarshaler{}, nil
	default:
		return nil, fmt.Errorf("unsupported metrics encoding %q", encoding)
	}
}

func newTracesUnmarshaler(encoding string) (ptrace.Unmarshaler, error) {
	switch encoding {
	case encodingOTLPProto:
		return &ptrace.ProtoUnmarshaler{}, nil
	case encodingOTLPJSON:
		return &ptrace.JSONUnmarshaler{}, nil
	default:
		return nil, fmt.Errorf("unsupported traces encoding %q", encoding)
	}
}

func (r *natsReceiver) Start(ctx context.Context, _ component.Host) error {
	meter := r.set.MeterProvider.Meter("go.datum.net/o11y/receiver/natsreceiver")

	lastDeliveryGauge, err := meter.Float64ObservableGauge(
		"natsreceiver_seconds_since_last_delivery",
		metric.WithDescription("Seconds since the last message was delivered to the pipeline. Resets to 0 on delivery."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			last := r.lastDelivery.Load()
			if last == 0 {
				return nil
			}
			o.Observe(time.Since(time.Unix(0, last)).Seconds())
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("creating seconds_since_last_delivery gauge: %w", err)
	}
	r.lastDeliveryGauge = lastDeliveryGauge

	resubscribeCount, err := meter.Int64Counter(
		"natsreceiver_resubscribe_total",
		metric.WithDescription("Count of JetStream consumer re-pulls triggered by a missed heartbeat (e.g. a consumer leader election)."),
	)
	if err != nil {
		return fmt.Errorf("creating resubscribe_total counter: %w", err)
	}
	r.resubscribeCount = resubscribeCount

	opts := []nats.Option{nats.Name(r.set.ID.String())}
	if r.cfg.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(r.cfg.CredentialsFile))
	}
	tlsCfg, err := r.cfg.TLS.LoadTLSConfig(ctx)
	if err != nil {
		return fmt.Errorf("loading tls config: %w", err)
	}
	if tlsCfg != nil {
		opts = append(opts, nats.Secure(tlsCfg))
	}

	conn, err := nats.Connect(r.cfg.URL, opts...)
	if err != nil {
		return fmt.Errorf("connecting to nats: %w", err)
	}
	r.conn = conn

	switch r.cfg.Stream {
	case "":
		return r.startCoreNATS(conn)
	default:
		return r.startJetStream(ctx, conn)
	}
}

func (r *natsReceiver) startCoreNATS(conn *nats.Conn) error {
	sub, err := conn.Subscribe(r.signal.Subject, r.handleCoreNATS)
	if err != nil {
		conn.Close()
		return fmt.Errorf("subscribing to %q: %w", r.signal.Subject, err)
	}
	r.coreNATSSub = sub
	return nil
}

func (r *natsReceiver) startJetStream(ctx context.Context, conn *nats.Conn) error {
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("creating jetstream context: %w", err)
	}
	cons, err := js.Consumer(ctx, r.cfg.Stream, r.cfg.ConsumerName)
	if err != nil {
		conn.Close()
		return fmt.Errorf("binding to consumer %q on stream %q: %w", r.cfg.ConsumerName, r.cfg.Stream, err)
	}
	consumeCtx, err := cons.Consume(r.handleJetStream, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
		// Consume's own heartbeat/re-pull logic has already recovered by the
		// time this fires (e.g. after a consumer leader election); record it
		// in case resubscribes become frequent enough to alert on.
		r.set.Logger.Warn("nats jetstream consume error, resubscribing", zap.Error(err))
		r.resubscribeCount.Add(ctx, 1)
	}))
	if err != nil {
		conn.Close()
		return fmt.Errorf("starting consume on %q: %w", r.cfg.ConsumerName, err)
	}
	r.consumeCtx = consumeCtx
	return nil
}

func (r *natsReceiver) handleCoreNATS(msg *nats.Msg) {
	// Core NATS has no ack/nak concept, so there's nothing to redeliver even
	// if delivery fails; deliver just logs and we move on.
	_ = r.deliver(context.Background(), msg.Data)
}

func (r *natsReceiver) handleJetStream(msg jetstream.Msg) {
	err := r.deliver(context.Background(), msg.Data())
	if err == nil {
		if ackErr := msg.Ack(); ackErr != nil {
			r.set.Logger.Error("acking nats jetstream message", zap.Error(ackErr))
		}
		return
	}

	var unmarshalErr *errUnmarshal
	if errors.As(err, &unmarshalErr) {
		// Non-retryable: this payload will never unmarshal, so ack (drop) it
		// rather than nak'ing it into an infinite, backoff-free redelivery
		// loop that would burn CPU and block real traffic. Logged at Error
		// since this is a deliberate, visible drop of an unprocessable message.
		r.set.Logger.Error("dropping unprocessable nats jetstream message (data loss)", zap.Error(err))
		if ackErr := msg.Ack(); ackErr != nil {
			r.set.Logger.Error("acking unprocessable nats jetstream message", zap.Error(ackErr))
		}
		return
	}

	// Retryable (e.g. a downstream export failure -- ClickHouse might
	// recover). NakWithDelay redelivers without the tight loop a bare Nak
	// would cause.
	if nakErr := msg.NakWithDelay(nakRedeliveryDelay); nakErr != nil {
		r.set.Logger.Error("nacking nats jetstream message", zap.Error(nakErr))
	}
}

func (r *natsReceiver) deliver(ctx context.Context, payload []byte) error {
	err := r.deliverPayload(ctx, payload)
	if err != nil {
		var unmarshalErr *errUnmarshal
		if errors.As(err, &unmarshalErr) {
			r.set.Logger.Error("unmarshaling nats payload", zap.Error(err))
		} else {
			r.set.Logger.Error("forwarding to next consumer", zap.Error(err))
		}
		return err
	}
	r.lastDelivery.Store(time.Now().UnixNano())
	return nil
}

func (r *natsReceiver) Shutdown(context.Context) error {
	if r.consumeCtx != nil {
		r.consumeCtx.Stop()
	}
	if r.coreNATSSub != nil {
		_ = r.coreNATSSub.Unsubscribe()
	}
	if r.conn != nil {
		return r.conn.Drain()
	}
	return nil
}
