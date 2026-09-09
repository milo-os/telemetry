# nkey-generator

Maintains the per-PoP NATS leaf NKey credentials for the o11y telemetry hub.

On each run (scheduled via a CronJob in the hub cluster) the generator:

1. Lists Karmada `Cluster` objects labelled `telemetry.miloapis.com/nats-leaf=enabled`,
   reaching the Karmada API directly with a secretless kubeconfig whose
   `tokenFile` points at a projected ServiceAccount token.
2. For each cluster, ensures a Secret `nats-leaf-nkey-<cluster>` exists
   (NKey seed + public key). A seed is **never regenerated** once stored — a
   regenerated seed would break that PoP until the next ESO refresh.
3. Maintains the ESO `PushSecret` that mirrors each seed Secret to GCP Secret
   Manager, so the edge PoP can pull it via `ExternalSecret`. The generator
   never talks to GCP; IAM stays with ESO.
4. Rewrites the `nats-o11y-authorized-leafs` ConfigMap. The hub's NATS
   `HelmRelease` consumes it via `valuesFrom`, so the per-PoP nkey users land
   in the hub's `O11Y` account (`config.merge.accounts.O11Y.users`). The
   generator is the single source of truth for that user list, including the
   static certificate-mapped client users (NACK, sink collector).

## Guards

- A Karmada API error aborts the whole run — a blip must never silently
  truncate the published user list.
- The generator refuses to write an empty user list.
- Users are removed only when their `Cluster` is present-and-unlabelled or
  absent, never due to a transient list failure.

## Build & test

```sh
task build     # CGO_ENABLED=0 go build
task test
task lint
```

## Image

Published to `ghcr.io/milo-os/telemetry-nkey-generator` by the repository's
`publish.yaml` workflow. Consumed by the CronJob deployed from
`datum-cloud/infra` (`apps/o11y-system/nkey-generator/`).
