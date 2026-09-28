# Shared compute node collector

An opt-in base for one collector per physical compute node. Infra composes this
base with provider-owned `config/components/node-telemetry` components. Nothing
in the existing collectors installation references this directory.

The base owns scheduling, RBAC, Kubernetes log reads, persistent offsets, and
export. Providers own their parsing, identity checks, and named pipelines. The
gateway continues to enforce the existing project routing contract.

## Component contract (version 1)

- Target `OpenTelemetryCollector/compute-node-collector` in `o11y-system`.
- Use the collector version in `manifest.json`; upgrade all components together.
- Give component IDs a service suffix, such as `filter/kata`.
- Add individual map entries and append mounts or environment variables. Do not
  replace shared maps or lists, create a second collector, or change shared RBAC.
- Reuse `filelog/kubernetes` for CRI logs. Add a receiver only for another source.
- Use `memory_limiter` first and `otlp_grpc/project` for logs. Set `project_name`
  from trusted Kubernetes metadata, not application log content.
- Use `prometheusremotewrite/compute` for metrics and `file_storage` for offsets.
- Declare owned pipelines in `manifest.json`. Secrets belong in `o11y-system`;
  a component must not include credentials.

`logs/platform` skips pods labeled `telemetry.miloapis.com/otlp-native-logs=true`
and records whose pod metadata is unavailable. Provider pipelines must reject
records without tenant identity. The gateway routes platform logs using the
namespace metadata forwarded by this base.

## Adoption

Infra must validate the assembled configuration with the pinned collector and
schedule it only on the chosen compute pool. Stop the existing generic node
agent and provider collector on those nodes before enabling this replacement;
overlap produces duplicates. Keep the generic agent on other nodes.

The new offset store starts from the beginning of retained files. Plan for
historical replay at cutover. Do not copy incompatible offset databases or
promise an atomic, lossless migration. Kubernetes metadata can disappear before
unread logs are attributed; those records are dropped. Delivery is not exactly
once, and the shared queue creates a shared capacity and failure boundary.

Provision the operator, namespace, gateway, metrics endpoint, provider secrets,
and host storage before rollout. The base uses root to read node log files and
write its hostPath state; infra must approve that access. Log export uses the
existing in-cluster plaintext OTLP endpoint; cluster network policy must restrict
access. This scaffold does not change authentication or query authorization.
