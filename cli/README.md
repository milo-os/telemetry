# Telemetry CLI

This module contains the `datumctl telemetry` plugin and the Go client it is
built on. Other CLIs and agents that read logs from the telemetry query API
import the same client.

## Plugin

The plugin binary is `milo-telemetry`. datumctl runs it as
`datumctl telemetry`.

```shell
# List the labels you can match on, then one label's values
datumctl telemetry labels
datumctl telemetry labels service_name --since 24h

# Show the newest 100 lines from the last hour, oldest first
datumctl telemetry logs '{service_name="checkout"}'

# Show every line containing "error" from the last day
datumctl telemetry logs '{service_name="checkout"} |= "error"' --since 24h --limit -1

# Keep printing new lines
datumctl telemetry logs '{service_name="checkout"}' -f
```

Run `datumctl telemetry logs --help` for every flag.

### Run a local build

```shell
task cli:build
PATH="$PWD/cli/bin:$PATH" DATUMCTL_TRUSTED_PLUGINS=telemetry datumctl telemetry labels
```

### Release

Every published GitHub release of this repository runs
`.github/workflows/release-plugin.yaml`, which does the following:

1. Builds the plugin archives with GoReleaser and attaches them to the release.
2. Pushes the tag `cli/<release tag>`, so Go can resolve this module at that
   version.
3. Opens a pull request that updates the plugin catalog in `milo-os/cli-plugins`.

## Go packages

Import path: `go.miloapis.com/telemetry/cli`

| Package | Purpose |
| --- | --- |
| `logs` | Client for the Loki-compatible endpoints, plus paging (`Tail`, `All`, `Pages`) and polling (`Follower`). |
| `logql` | Builds selectors and line filters with correct quoting and regex escaping. |

These packages depend only on the Go standard library. Each consumer keeps
its own label vocabulary and output format.

```go
import (
	"go.datum.net/datumctl/plugin"
	"go.miloapis.com/telemetry/cli/logql"
	"go.miloapis.com/telemetry/cli/logs"
)

client := logs.New(logs.ProjectURL(plugin.Context().APIHost, project),
	logs.WithToken(plugin.Token))

var sel logql.Selector
sel.Eq("datum_workload_name", workload).OneOf("log_iostream", "stderr").Contains("panic")

entries, err := logs.Tail(ctx, client, sel.String(), time.Now().Add(-time.Hour), time.Now(), 200)
```

A caller that already has a client-go `rest.Config` passes
`logs.WithHTTPClient(httpClient)` instead of `WithToken`. `rest.HTTPClientFor`
authenticates the requests.

`*logs.APIError` carries the HTTP status and the server's reason. `Hint()`
suggests a next step for the user, and `logs.Permanent(err)` reports whether a
retry can help.
