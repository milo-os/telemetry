// SPDX-License-Identifier: AGPL-3.0-only

// Command queryapi serves the telemetry query layer's API. It is a Kubernetes
// aggregated API server that serves Loki- and Prometheus-shaped routes rather
// than Kubernetes resources; see ../openapi.yaml for the request/response
// contract and ../README.md for how requests are authorized.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/pflag"

	"go.datum.net/o11y/queryapi"
	"go.datum.net/o11y/queryapi/internal/storage"
	"go.datum.net/o11y/queryapi/internal/storage/clickhouse"
	"go.datum.net/o11y/queryapi/internal/storage/fake"
	"go.datum.net/o11y/queryapi/internal/storage/prometheus"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger, os.Args[1:]); err != nil {
		logger.Error("query api exited", "error", err)
		os.Exit(1)
	}
}

// run holds the whole process so that every deferred close still happens; main
// only turns the error into an exit code.
func run(logger *slog.Logger, args []string) error {
	opts := queryapi.NewOptions()
	fs := pflag.NewFlagSet("queryapi", pflag.ExitOnError)
	opts.AddFlags(fs)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if err := opts.Complete(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if err := opts.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	logStore, err := newLogStore(opts.Query)
	if err != nil {
		return fmt.Errorf("configure storage: %w", err)
	}

	metricStore, err := newMetricStore(opts.Query)
	if err != nil {
		return fmt.Errorf("configure metrics storage: %w", err)
	}

	cfg, err := opts.Config(logger, logStore, metricStore)
	if err != nil {
		return err
	}
	completed, err := cfg.Complete()
	if err != nil {
		return err
	}
	server, err := completed.New()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("serving query api", "port", opts.Recommended.SecureServing.BindPort)
	return server.Run(ctx)
}

// newLogStore builds the configured log storage backend.
func newLogStore(cfg queryapi.Config) (storage.LogStore, error) {
	switch cfg.LogsStorage {
	case "fake":
		return fake.NewLogStore(cfg.FakeRate), nil
	case "clickhouse":
		chCfg, err := clickhouse.ConfigFromEnv()
		if err != nil {
			return nil, err
		}
		return clickhouse.New(chCfg)
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.LogsStorage)
	}
}

// newMetricStore builds the configured metric backend.
func newMetricStore(cfg queryapi.Config) (storage.MetricStore, error) {
	switch cfg.MetricsStorage {
	case "fake":
		return fake.NewMetricStore(), nil
	case "prometheus":
		tenancy, err := prometheus.ParseTenancy(cfg.PrometheusTenancy)
		if err != nil {
			return nil, err
		}
		return prometheus.New(prometheus.Config{
			URL:              cfg.PrometheusURL,
			Tenancy:          tenancy,
			ProjectLabel:     cfg.PrometheusProjectLabel,
			MaxResponseBytes: cfg.MaxResponseBytes,
		})
	default:
		return nil, fmt.Errorf("unknown metrics storage backend %q", cfg.MetricsStorage)
	}
}
