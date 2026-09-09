package natsexporter

import (
	"context"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// startTestServer starts an in-process NATS server with JetStream enabled
// on a random port, and tears it down at the end of the test.
func startTestServer(t *testing.T) *natsserver.Server {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server did not become ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// createTestStream connects to srv and creates a JetStream stream covering
// the given subjects, returning a JetStream context usable for consuming
// published messages back out in tests.
func createTestStream(t *testing.T, srv *natsserver.Server, stream string, subjects ...string) jetstream.JetStream {
	t.Helper()
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     stream,
		Subjects: subjects,
	})
	require.NoError(t, err)
	return js
}

// consumeOne creates an ephemeral consumer on subject and returns the
// payload of the next message published to it.
func consumeOne(t *testing.T, js jetstream.JetStream, stream, subject string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cons, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	require.NoError(t, err)

	var payload []byte
	for msg := range msgs.Messages() {
		payload = msg.Data()
		require.NoError(t, msg.Ack())
	}
	require.NoError(t, msgs.Error())
	require.NotNil(t, payload, "expected one message on subject %q", subject)
	return payload
}

func testConfig(t *testing.T, srv *natsserver.Server) *Config {
	t.Helper()
	cfg := createDefaultConfig().(*Config)
	cfg.URL = srv.ClientURL()
	cfg.Stream = "otlp"
	cfg.TLS.Insecure = true
	require.NoError(t, cfg.Validate())
	return cfg
}

// TestPublishToDomainConfiguredServerNeedsNoDomainClient confirms that an
// acked JetStream publish with no domain configured on the client lands in a
// stream on a server that *does* configure a JetStream domain. js.Publish only
// writes to the data subject and awaits a PubAck; it makes no $JS.API call and
// so does not need the domain to address the API. This is the property that
// lets the edge exporter publish into the hub's domain with just `stream` set.
func TestPublishToDomainConfiguredServerNeedsNoDomainClient(t *testing.T) {
	opts := &natsserver.Options{
		Host:            "127.0.0.1",
		Port:            -1,
		JetStream:       true,
		JetStreamDomain: "hub",
		StoreDir:        t.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server did not become ready")
	}
	t.Cleanup(srv.Shutdown)

	js := createTestStream(t, srv, "o11y", "o11y.>")

	cfg := createDefaultConfig().(*Config)
	cfg.URL = srv.ClientURL()
	cfg.Stream = "o11y"
	cfg.TLS.Insecure = true
	cfg.Logs.Subject = "o11y.logs.test-cluster.test-project"

	exp, err := newExporter(cfg, exportertest.NewNopSettings(exportertest.NopType))
	require.NoError(t, err)
	require.NoError(t, exp.start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, exp.shutdown(t.Context())) })

	logs := plog.NewLogs()
	logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	require.NoError(t, exp.pushLogs(t.Context(), logs))

	payload := consumeOne(t, js, "o11y", "o11y.logs.test-cluster.test-project")
	assert.NotEmpty(t, payload)
}

func TestExporter_PushLogs(t *testing.T) {
	srv := startTestServer(t)
	js := createTestStream(t, srv, "otlp", "otlp.>")
	cfg := testConfig(t, srv)

	exp, err := newExporter(cfg, exportertest.NewNopSettings(exportertest.NopType))
	require.NoError(t, err)
	require.NoError(t, exp.start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, exp.shutdown(t.Context())) })

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "test")
	rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("hello")

	require.NoError(t, exp.pushLogs(t.Context(), ld))

	payload := consumeOne(t, js, "otlp", cfg.Logs.Subject)
	got, err := (&plog.ProtoUnmarshaler{}).UnmarshalLogs(payload)
	require.NoError(t, err)
	assert.Equal(t, 1, got.LogRecordCount())
	assert.Equal(t, "hello", got.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Body().Str())
}

func TestExporter_PushLogs_CoreNATSMode(t *testing.T) {
	srv := startTestServer(t)
	cfg := testConfig(t, srv)
	cfg.Stream = ""

	exp, err := newExporter(cfg, exportertest.NewNopSettings(exportertest.NopType))
	require.NoError(t, err)
	require.NoError(t, exp.start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, exp.shutdown(t.Context())) })

	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	sub, err := nc.SubscribeSync(cfg.Logs.Subject)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sub.Unsubscribe()) })
	// SubscribeSync returns once the SUBSCRIBE is queued client-side, not
	// once the server has processed it -- without this flush, exp's publish
	// (a separate connection) can race ahead of it, and core NATS doesn't
	// replay for a subscriber that wasn't registered yet.
	require.NoError(t, nc.Flush())

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("hello")

	require.NoError(t, exp.pushLogs(t.Context(), ld))

	msg, err := sub.NextMsg(5 * time.Second)
	require.NoError(t, err)
	got, err := (&plog.ProtoUnmarshaler{}).UnmarshalLogs(msg.Data)
	require.NoError(t, err)
	assert.Equal(t, 1, got.LogRecordCount())
	assert.Equal(t, "hello", got.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Body().Str())
}

func TestExporter_PushMetrics(t *testing.T) {
	srv := startTestServer(t)
	js := createTestStream(t, srv, "otlp", "otlp.>")
	cfg := testConfig(t, srv)

	exp, err := newExporter(cfg, exportertest.NewNopSettings(exportertest.NopType))
	require.NoError(t, err)
	require.NoError(t, exp.start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, exp.shutdown(t.Context())) })

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("cpu.usage")
	m.SetEmptyGauge().DataPoints().AppendEmpty().SetDoubleValue(42)

	require.NoError(t, exp.pushMetrics(t.Context(), md))

	payload := consumeOne(t, js, "otlp", cfg.Metrics.Subject)
	got, err := (&pmetric.ProtoUnmarshaler{}).UnmarshalMetrics(payload)
	require.NoError(t, err)
	assert.Equal(t, 1, got.DataPointCount())
	assert.Equal(t, "cpu.usage", got.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Name())
}

func TestExporter_PushTraces(t *testing.T) {
	srv := startTestServer(t)
	js := createTestStream(t, srv, "otlp", "otlp.>")
	cfg := testConfig(t, srv)

	exp, err := newExporter(cfg, exportertest.NewNopSettings(exportertest.NopType))
	require.NoError(t, err)
	require.NoError(t, exp.start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, exp.shutdown(t.Context())) })

	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("test-span")

	require.NoError(t, exp.pushTraces(t.Context(), td))

	payload := consumeOne(t, js, "otlp", cfg.Traces.Subject)
	got, err := (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(payload)
	require.NoError(t, err)
	assert.Equal(t, 1, got.SpanCount())
	assert.Equal(t, "test-span", got.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Name())
}

func TestExporter_SubjectFromAttribute(t *testing.T) {
	srv := startTestServer(t)
	js := createTestStream(t, srv, "otlp", "otlp.>", "tenant.>")
	cfg := testConfig(t, srv)
	cfg.SubjectFromAttribute = "tenant.subject"

	exp, err := newExporter(cfg, exportertest.NewNopSettings(exportertest.NopType))
	require.NoError(t, err)
	require.NoError(t, exp.start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, exp.shutdown(t.Context())) })

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("tenant.subject", "tenant.acme")
	rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("hello")

	require.NoError(t, exp.pushLogs(t.Context(), ld))

	payload := consumeOne(t, js, "otlp", "tenant.acme")
	got, err := (&plog.ProtoUnmarshaler{}).UnmarshalLogs(payload)
	require.NoError(t, err)
	assert.Equal(t, 1, got.LogRecordCount())
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(*Config) {}, false},
		{"missing url", func(c *Config) { c.URL = "" }, true},
		{"missing stream is valid (core NATS mode)", func(c *Config) { c.Stream = "" }, false},
		{"bad logs encoding", func(c *Config) { c.Logs.Encoding = "bogus" }, true},
		{"missing metrics subject", func(c *Config) { c.Metrics.Subject = "" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := createDefaultConfig().(*Config)
			cfg.URL = "nats://localhost:4222"
			cfg.Stream = "otlp"
			tt.mutate(cfg)
			err := cfg.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNewMarshalers_UnsupportedEncoding(t *testing.T) {
	_, err := newLogsMarshaler("bogus")
	assert.Error(t, err)
	_, err = newMetricsMarshaler("bogus")
	assert.Error(t, err)
	_, err = newTracesMarshaler("bogus")
	assert.Error(t, err)
}
