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
	if err := opts.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	store, err := newStore(opts.Query)
	if err != nil {
		return fmt.Errorf("configure storage: %w", err)
	}

	cfg, err := opts.Config(logger, store)
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

// newStore builds the configured storage backend.
func newStore(cfg queryapi.Config) (storage.LogStore, error) {
	switch cfg.Storage {
	case "fake":
		return fake.New(cfg.FakeRate), nil
	case "clickhouse":
		chCfg, err := clickhouse.ConfigFromEnv()
		if err != nil {
			return nil, err
		}
		return clickhouse.New(chCfg)
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.Storage)
	}
}
