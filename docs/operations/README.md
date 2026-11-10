# Operations

This page covers what to watch in the telemetry pipeline, runbooks for two
failure modes, and standing correctness checks. The repository doesn't ship
alert rules. A deployment defines its own, so this page describes what each
signal detects.

- [Signals to watch](#signals-to-watch)
- [Runbooks](#runbooks)
  - [Records land under `internal`](#records-land-under-internal)
  - [ClickHouse hot volume is filling](#clickhouse-hot-volume-is-filling)
- [Correctness checks](#correctness-checks)
- [ClickHouse client shell](#clickhouse-client-shell)
- [Scope](#scope)

## Signals to watch

A deployment typically alerts on the following conditions.

- **Sink consumer:** the sink's JetStream consumer has a growing backlog of
  pending messages. At a critical level, the hub stream risks aging out unread
  messages.
- **Sink delivery:** the sink discards messages it couldn't insert into
  ClickHouse. The sink acks before the insert succeeds, so discarded messages
  are lost ([telemetry#235](https://github.com/milo-os/telemetry/issues/235)).
- **Collectors:** a collector drops records it failed to export, or refuses
  incoming records.
- **Credentials:** the job that rotates edge NATS credentials
  (`nkey-generator`) is failing.
- **ClickHouse:** too many parts per partition, failed inserts, or replication
  lag between replicas.
- **ClickHouse disk:** the hot volume's persistent volume is close to full.
- **Export:** per-policy sink errors or discarded events from the exporter, and
  exporter availability. The exporter is one replica that serves every policy.

The exporter's per-policy metrics are described in
[export policies](../architecture/export-policies.md#exporter-metrics).

## Runbooks

### Records land under `internal`

Attribution doesn't drop records that lack a project. The gateway collector's
`transform/milo_project_id` processor assigns `milo.project.id` from the first
tier that applies:

1. The `project_name` resource attribute.
2. The namespace label `resourcemanager.miloapis.com/project-name`.
3. The namespace label `meta.datumapis.com/upstream-cluster-name`, only when
   its value starts with `cluster-`. The prefix is stripped.
4. The literal `internal`.

A tenant workload that matches none of the first three tiers doesn't error.
Its records publish to `o11y.logs.<cluster>.internal` and land in the
operator-only `internal` path, so the tenant can't see them. The sink consumes
`o11y.logs.>` only, so this applies to logs.

To find a misattributed source:

1. Identify the source by `service.namespace` or `k8s.cluster.name` on the
   `internal`-routed records.
2. Check whether the namespace carries
   `resourcemanager.miloapis.com/project-name` or
   `meta.datumapis.com/upstream-cluster-name`.
3. If neither label is present, the namespace's owner needs to add one. If a
   label is present but the records still land under `internal`, check that the
   gateway collector's `k8sattributes` processor extracts the label.

See [ingest pipeline: attribution and per-project routing](../architecture/ingest-pipeline.md#attribution-and-per-project-routing)
and [logs: resource attribute contract](../architecture/logs.md#resource-attribute-contract).

### ClickHouse hot volume is filling

At 0% free space ClickHouse stops accepting inserts, which stalls the sink
consumer and backs the hub stream up. The hub stream has a 24-hour age limit
and a `maxBytes` cap. Once either is exceeded, the oldest messages are dropped
and the data is lost.

1. **Find what uses the space.** The hot storage policy volume is named
   `default`, so its disk is `default`, not `hot`. In ClickHouse:

   ```sql
   SELECT database, table,
          formatReadableSize(sum(bytes_on_disk)) AS size
   FROM system.parts
   WHERE disk_name = 'default' AND active
   GROUP BY database, table
   ORDER BY sum(bytes_on_disk) DESC
   LIMIT 20;
   ```

   Check whether growth is uniform or concentrated in one table or project.
   Include the `system.*` tables: ClickHouse's own log tables can grow by
   gigabytes per day under the operator's default retention, and
   `system.text_log` may have no TTL at all. See
   [retention: ClickHouse system logs](../architecture/retention.md#clickhouse-system-logs).

2. **Force a TTL merge.** ClickHouse applies TTL lazily, so expired data can
   stay on disk until the next background merge.

   ```sql
   OPTIMIZE TABLE o11y.logs FINAL;
   ```

   Check free space after the merge. If it recovers significant space, consider
   tuning `merge_with_ttl_timeout`.

3. **Resize the volume** if it's undersized and the storage class supports
   online expansion. In the ClickHouseInstallation, raise the storage request
   on the data volume claim template (`data-volume`). Don't set a size below
   current usage.

   ```yaml
   spec:
     templates:
       volumeClaimTemplates:
         - name: data-volume
           spec:
             resources:
               requests:
                 storage: <new-size>
   ```

4. **Add a shard, not a replica,** if one node's disk ceiling is reached. The
   cluster is one shard with three replicas, and a new replica copies all the
   data without adding capacity. A new shard needs a distributed table layout,
   which the current schema doesn't use.

5. **Don't lower the cold-tier threshold for logs** as a short-term fix. Tenant
   logs stay on the hot volume for their 7-day life so that cold-read latency
   doesn't affect queries. `internal` logs move to the cold volume after one
   day (see
   [`000003_log_retention.up.sql`](../../config/clickhouse-migrations/migrations/000003_log_retention.up.sql)).

See [retention: storage policy](../architecture/retention.md#storage-policy).

## Correctness checks

These standing checks verify properties that the platform doesn't enforce by
itself. Each would fail silently, or look like normal operation, if left
unchecked. Rerun them after any change to the components involved.

### JetStream consumer leader election doesn't stall ingest

**What to verify:** Force a consumer leader election on a clustered hub while
the sink collector is consuming. Confirm delivery continues without manual
intervention. Watch for `natsreceiver_resubscribe_total` to increment when the
hub logs the new leader.

**Why it matters:** This failure retired the `ENGINE=NATS` ingest-table design.
It bound to the consumer, survived restarts, then stopped consuming after a
leader election while still logging success. See
[ingest pipeline: ClickHouse consumer](../architecture/ingest-pipeline.md#clickhouse-consumer).
A hub with more than one replica hits leader elections routinely, so a
regression is an outage. Rerun the check after any change to `o11y-sink` or the
hub's replica count.

**If it fails:** Treat a stall as a receiver bug. Don't add a restart loop.

### An unscoped tenant query returns no rows

**What to verify:** Connect to ClickHouse as the query layer's own certificate
identity, without setting `telemetry_project_id`, and query `o11y.logs`. Confirm
the query returns zero rows. Then confirm the query layer's generated SQL
carries an explicit `ProjectId = ?` predicate, so the row policy isn't the only
control between a caller and another tenant's data.

**Why it matters:** The failure mode is silence, not an exception.
`getSetting('telemetry_project_id')` returns the configured default when a query
never set it. The row policy then matches nothing, which looks the same as "no
logs matched". If a real project ID were configured as the default, the policy
would match that project's rows for every session. Neither case errors, so a
test that only asserts "the query threw" passes on a broken configuration.

Enforcement is doubled for this reason: the explicit predicate and the
backends' own project check make an unscoped query impossible, and the row
policy is the backstop. The check confirms both layers.

The row policy is `ProjectId = getSetting('telemetry_project_id')`
([`000001_init.up.sql`](../../config/clickhouse-migrations/migrations/000001_init.up.sql)).
It applies to the ClickHouse user named by `CLICKHOUSE_QUERYAPI_USER`, which
the migration takes as a parameter. The default is `queryapi`
([`clickhouse-migrate/main.go`](../../clickhouse-migrate/main.go)). Set it to
the user named by the query layer's certificate common name.
`custom_settings_prefixes` must register the `telemetry_` prefix.

**If it fails:** Confirm that `CLICKHOUSE_QUERYAPI_USER` in the migrations Job
matches the query layer's actual ClickHouse user, and that
`telemetry_project_id` is declared with no default value.

See [query layer: tenant enforcement](../architecture/query-layer.md#tenant-enforcement)
and [logs: ClickHouse schema](../architecture/logs.md#clickhouse-schema).

## ClickHouse client shell

To connect to ClickHouse interactively, for example to recover from a dirty
migration, use the `ch-shell` Pod in
[`clickhouse-migrate/ch-shell.yaml`](../../clickhouse-migrate/ch-shell.yaml).
It mounts a short-lived client certificate for the migrations user. The Pod
holds live credentials, so delete it when you finish. The
[`clickhouse-migrate` README](../../clickhouse-migrate/README.md#creating-a-client-shell)
has the commands.

## Scope

Infrastructure-level alerts, such as NATS cluster health, ClickHouse
replication lag, and object-store availability, belong in the deployment's own
runbooks.
