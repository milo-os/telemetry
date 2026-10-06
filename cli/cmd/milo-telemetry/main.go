// SPDX-License-Identifier: AGPL-3.0-only

// Command milo-telemetry is the datumctl telemetry plugin. It reads logs back
// from the project's telemetry query API.
package main

import (
	"context"
	"os"
	"os/signal"

	"go.datum.net/datumctl/plugin"

	"go.miloapis.com/telemetry/cli/internal/command"
)

// pluginVersion is overridden at release time by goreleaser via
// -ldflags "-X main.pluginVersion=<version>".
var pluginVersion = "0.0.0"

func main() {
	// Serve --plugin-manifest before cobra runs, so it works even if flag
	// parsing would fail.
	plugin.ServeManifest(plugin.Manifest{
		Name:        "telemetry",
		Version:     pluginVersion,
		Description: "Query logs for the active project",
		// The datumctl <-> plugin contract version, not an API group version.
		APIVersion:    1,
		MinAPIVersion: 1,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	streams := command.Streams{Out: os.Stdout, ErrOut: os.Stderr}
	root := command.NewRoot(streams, command.DatumctlClient("milo-telemetry/"+pluginVersion))
	root.Version = pluginVersion
	if err := root.ExecuteContext(ctx); err != nil {
		command.Render(os.Stderr, err)
		stop()
		os.Exit(1)
	}
}
