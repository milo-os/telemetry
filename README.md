# Telemetry Services

A Kubernetes operator and supporting workloads for managing telemetry on the
Datum Cloud platform. It provides a standardized way to configure and manage
telemetry exporters, ensuring seamless integration with observability platforms
through OpenTelemetry.

This repository is a multi-module Go workspace. Each module lives in its own
directory with its own `go.mod`, and every module exposes its own `task` tasks
for building, testing, linting, and deploying. The root `Taskfile.yaml` wires
them together.

## Repository layout

| Path | Module | Description |
| --- | --- | --- |
| `operator/` | `go.datum.net/o11y/operator` | The operator: a Kubernetes controller that reconciles `ExportPolicy` CRs (`telemetry.miloapis.com/v1alpha1`). |
| `clickhouse-migrate/` | `go.datum.net/o11y/clickhouse-migrate` | Applies SQL migrations to ClickHouse. Runs as a Kubernetes Job. |
| `receiver/natsreceiver/` | `go.datum.net/o11y/receiver/natsreceiver` | OpenTelemetry collector receiver that reads logs from a NATS JetStream stream (used by the sink collector). |
| `exporter/natsexporter/` | `go.datum.net/o11y/exporter/natsexporter` | OpenTelemetry collector exporter that publishes logs to NATS. |
| `processor/unbatchprocessor/` | `go.datum.net/o11y/processor/unbatchprocessor` | OpenTelemetry collector processor that expands a batch back into one record per message. |
| `otelcol/` | | OCB (opentelemetry collector builder) manifest + Dockerfile for the custom `o11y/otelcol` distribution that the collector CRs run on. |
| `config/` | | Kustomize bundles published as an OCI artifact and consumed by Flux. |

## Prerequisites

- Go 1.24+ (see each module's `go.mod` for the exact version)
- [task](https://taskfile.dev) v3
- `kubectl` and access to a Kubernetes cluster
- `docker` for building images and running the e2e/integration tests

## Building and testing

The root `Taskfile.yaml` aggregates the per-module tasks:

```sh
task build   # build every module that has its own build task
task test    # run every module's unit/controller tests
task lint    # run golangci-lint in every module
task tidy    # run go mod tidy in every module (mirrors CI)

# Everything CI runs before a push. Note: includes the operator e2e suite,
# which requires a running Kind cluster.
task ci
```

Individual modules can be targeted directly via their include aliases, e.g.:

```sh
task operator:test
task clickhouse-migrate:test
task natsreceiver:test
task natsexporter:test
task unbatchprocessor:test
```

The operator module has the richest task set (run `task operator --list` for the
full list). Highlights:

```sh
task operator:tidy
task operator:test             # manifests + generate + fmt + vet + envtest unit tests
task operator:test-e2e         # full e2e suite; expects a running Kind cluster
task operator:lint
task operator:api-docs         # regenerate CRD API reference docs into operator/docs/api
```

## Deploying the operator

The operator is installed and deployed with `task` targets run from the
`operator/` directory (or via `task operator:<target>` from the root).

**Install the CRDs into the cluster:**

```sh
task operator:install
```

**Deploy the manager to the cluster:**

```sh
task operator:deploy
```

The `deploy`/`build-installer` tasks stamp the operator image on a scratch copy
of the kustomization, so the tracked `config/operator/manager/kustomization.yaml`
is never mutated. By default the image resolves to `controller:latest`; override
with `IMAGE_NAME`/`IMAGE_TAG`, e.g.:

```sh
task operator:deploy IMAGE_NAME=ghcr.io/milo-os/telemetry IMAGE_TAG=<tag>
```

**Apply a sample `ExportPolicy`:**

```sh
kubectl apply -k config/operator/samples/
```

**To uninstall:**

```sh
task operator:undeploy   # remove the manager
task operator:uninstall  # remove the CRDs
```

## Collector bundles

`config/collectors/` holds the OpenTelemetryCollector CRs that run on the custom
`o11y/otelcol` distribution:

- `gateway-collector` (deployment) — centralized OTLP push receiver, publishes logs to NATS.
- `node-agent-collector` (daemonset) — node-local agent that tails pod logs and publishes to NATS.
- `o11y-sink-collector` (hub-side deployment) — reads the hub NATS JetStream consumer and writes logs to ClickHouse.

The collector image tags are stamped at publish time by the release workflow (see
below); the in-repo manifests carry a `v0.0.0-dev` placeholder.

## Releases and distribution

On release, `.github/workflows/publish.yaml`:

1. Builds and publishes the operator image (`ghcr.io/milo-os/telemetry`), the
   clickhouse-migrate image (`ghcr.io/milo-os/telemetry-clickhouse-migrate`),
   and the otelcol image (`ghcr.io/milo-os/o11y/otelcol`).
2. Publishes the `config/` tree as a kustomize bundle OCI artifact
   (`ghcr.io/milo-os/o11y/kustomize`), stamping the release image tags into each
   overlay via Flux's `publish-kustomize-bundle` workflow.

Consumers (e.g. Flux `OCIRepository`) pull the bundle and apply it with the
released image tags baked in.

## Documentation

- ClickHouse migrations: see `clickhouse-migrate/README.md`.
- Operator CRD API reference: see `operator/docs/api/`.
