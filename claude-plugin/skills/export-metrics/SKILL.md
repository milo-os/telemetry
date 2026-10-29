---
name: export-metrics
description: Export a Datum Cloud project's metrics to Grafana Cloud or any Prometheus remote-write endpoint with an ExportPolicy. Use when someone wants Datum metrics in Grafana, Prometheus, Mimir, or another metrics tool, or asks why an export isn't working.
---

# Export metrics to Grafana Cloud or Prometheus remote write

Datum keeps the metrics for a project in the project's metrics store. An
`ExportPolicy` ships a selection of them to an endpoint that accepts Prometheus
remote write, such as Grafana Cloud. Once they arrive, query them with the
`status-metrics` skill or Datum's Grafana dashboards.

The Cloud Portal can also set this up: open the project, go to the **Metrics**
tab, and follow the steps. Use the steps below when working from the terminal
or keeping the policy in Git.

## API

- Group and version: `telemetry.miloapis.com/v1alpha1`. **Alpha**, so field
  names may change between releases.
- Kind: `ExportPolicy` (plural `exportpolicies`), namespaced, in a project.

| Field | Required | Notes |
| --- | --- | --- |
| `spec.sources[]` | yes, 1 to 20 | Each has a unique `name` (a DNS label). |
| `spec.sources[].metrics.metricsql` | yes | A MetricsQL **selector** such as `{__name__=~"envoy_.*"}`. See the rules below. |
| `spec.sinks[]` | yes, 1 to 20 | Each has a unique `name` (a DNS label). |
| `spec.sinks[].sources[]` | yes | Names from `spec.sources`. |
| `spec.sinks[].target.prometheusRemoteWrite.endpoint` | yes | The remote-write URL. |
| `...prometheusRemoteWrite.authentication.basicAuth.secretRef.name` | no | A Secret in the same namespace. See step 1. |
| `...prometheusRemoteWrite.batch` | defaulted | `maxSize` 1 to 5000 (default 500), `timeout` (default `5s`). |
| `...prometheusRemoteWrite.retry` | defaulted | `maxAttempts` 1 to 10 (default 3), `backoffDuration` (default `5s`). |

### Rules for the `metricsql` selector

The admission webhook rejects a policy that breaks these, so check them before
applying:

- Every source needs `metrics.metricsql`. A source with no `metrics` is
  rejected. To export **everything** in the project, use `{__name__=~".+"}`.
- It must be a plain series selector, `{label="value", ...}` or
  `metric_name{...}`. Functions and aggregations such as `rate(...)` or
  `sum by (...)` are rejected.
- Don't filter on `project_name`. Datum adds the project filter itself, so a
  policy can only ever export its own project's metrics.

## Steps

### 1. Store the endpoint's credentials in a Secret

The Secret **must** be of type `kubernetes.io/basic-auth` and have `username`
and `password` keys. Any other type, or a missing key, fails the sink with
reason `InvalidAuthentication`.

For Grafana Cloud, the username is the Prometheus instance ID and the password
is an access policy token with the `metrics:write` scope. Both are on the
stack's **Prometheus** details page, next to the remote-write URL.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: grafana-cloud-credentials
type: kubernetes.io/basic-auth
stringData:
  username: "123456"
  password: "glc_..."
```

```bash
datumctl apply -f grafana-secret.yaml
```

Ask the user for the token, or have them write the file themselves. Don't paste
a token into chat output, a commit, or the ExportPolicy.

### 2. Write the policy

Export everything:

```yaml
apiVersion: telemetry.miloapis.com/v1alpha1
kind: ExportPolicy
metadata:
  name: grafana-cloud-export
spec:
  sources:
    - name: all-metrics
      metrics:
        metricsql: '{__name__=~".+"}'
  sinks:
    - name: grafana-cloud
      sources:
        - all-metrics
      target:
        prometheusRemoteWrite:
          endpoint: https://prometheus-prod-01-prod-us-east-0.grafana.net/api/prom/push
          authentication:
            basicAuth:
              secretRef:
                name: grafana-cloud-credentials
          batch:
            maxSize: 500
            timeout: 5s
          retry:
            maxAttempts: 3
            backoffDuration: 5s
```

Export only some metrics by narrowing the selector. Useful sources:

- Load balancer traffic and firewall:
  `{__name__=~"envoy_vhost_vcluster_upstream_rq.*|coraza_envoy_filter_request_events_total"}`
- Compute Instance CPU and memory: `{__name__=~"datum_compute_instance_.*"}`
- Status of every Datum resource: `{__name__=~"datum_cloud_.*|dns_.*"}`

Several sources can feed one sink, and one source can feed several sinks. Series
that match more than one source are sent once per source, not de-duplicated.

### 3. Preview and apply

```bash
datumctl apply -f exportpolicy.yaml --dry-run=server
datumctl diff -f exportpolicy.yaml
datumctl apply -f exportpolicy.yaml
```

`--dry-run=server` runs the admission webhook, so it catches a bad selector
without changing anything.

### 4. Check it

```bash
datumctl get exportpolicy grafana-cloud-export -o yaml
```

- `status.conditions` has one `Ready` condition. `True` (reason
  `SinksAccepted`) means every sink was accepted. `False` (reason
  `SinksNotAccepted`) says how many were.
- `status.sinks[].conditions` has an `Accepted` condition per sink. `False` with
  reason `InvalidAuthentication` means the Secret is missing, the wrong type, or
  lacks `username` or `password`; the message says which.

`Accepted` only means the configuration is valid. It doesn't prove the endpoint
took the data. To confirm delivery, query the destination after a minute or two,
for example `count({resourcemanager_datumapis_com_project_name="PROJECT_ID"})`
in Grafana Cloud's Explore view. If nothing arrives, check the endpoint URL and
that the token has write access. Delivery isn't guaranteed while the endpoint is
down or misconfigured: after `retry.maxAttempts`, a batch is dropped.

## Other operations

```bash
datumctl get exportpolicies
datumctl delete exportpolicy NAME
```

`delete` doesn't ask for confirmation. Confirm the name with the user first.
Deleting a policy stops the export but leaves its Secret in place.

## Datum dashboards for Grafana Cloud

In Grafana Cloud, go to **Dashboards**, **New**, **Import**, and load these IDs:

- **23939**, Datum Overview: load balancer, hostname, and domain counts,
  requests per second by region, response codes, latency, and health.
- **24261**, Datum Proxy: one load balancer's hostnames and regional traffic.

## See also

- [Metrics export](https://www.datum.net/docs/operations/metrics-export) in the
  Datum docs
- [MetricsQL](https://docs.victoriametrics.com/victoriametrics/metricsql/)
- [Grafana Cloud remote write](https://grafana.com/docs/grafana-cloud/send-data/metrics/metrics-prometheus/)
