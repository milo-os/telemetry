---
name: status-metrics
description: Read and query the status metrics Datum Cloud publishes for every resource (the _info and _status_condition series) in Grafana or another metrics tool. Use when someone wants to count resources, alert on resources that aren't ready, build a dashboard of resource health, or asks what a Datum metric means.
---

# Read Datum resource status metrics

Besides traffic and usage metrics, Datum publishes the **state** of every
resource in a project as metrics: whether a load balancer is programmed, a
domain verified, a workload available, a DNS record set serving. They answer
"what's broken right now?" across a whole project in one query, and they're what
to alert on.

These metrics live in the project's metrics store. To query them in Grafana or
another tool, export them first (see the `export-metrics` skill). The Cloud
Portal charts them on each resource's pages.

## The pattern

Every resource kind publishes the same families, named after the resource:

| Family | What it reports |
| --- | --- |
| `<resource>_info` | One series per resource, value `1`, with descriptive labels. Count resources or join in labels. |
| `<resource>_status_condition` | One series per condition, `1` when the condition is true and `0` when it isn't. Labels `condition`, `status`, and `reason`. |
| `<resource>_status_condition_last_transition_time` | When each condition last changed, as Unix seconds. |
| `<resource>_created` | When the resource was created, as Unix seconds. |
| `<resource>_owner` | One series per owner reference: `owner_kind`, `owner_name`. |

`<resource>` is a prefix such as `datum_cloud_networking_http_proxy` or
`dns_record_set`. [reference/metric-catalog.md](reference/metric-catalog.md)
lists every family by product, with its extra labels and the conditions to
watch. Not every kind has every family.

### Labels on every series

| Label | Meaning |
| --- | --- |
| `resourcemanager_datumapis_com_project_name` | The project ID. Include it in every query, because resource names are only unique within a project. |
| `resource_name` | The resource's name, as `datumctl` shows it. |
| `resource_namespace` | The resource's namespace, usually `default`. |

## Queries

Replace `PROJECT_ID`. Queries use
[MetricsQL](https://docs.victoriametrics.com/victoriametrics/metricsql/), which
also accepts standard PromQL.

Load balancers that aren't programmed, with the reason:

```promql
datum_cloud_networking_http_proxy_status_condition{resourcemanager_datumapis_com_project_name="PROJECT_ID", condition="Programmed"} == 0
```

Domains that aren't verified yet:

```promql
datum_cloud_networking_domain_status_condition{resourcemanager_datumapis_com_project_name="PROJECT_ID", condition="Verified"} == 0
```

DNS record sets that aren't serving:

```promql
dns_record_set_status_condition{resourcemanager_datumapis_com_project_name="PROJECT_ID", condition="Programmed"} == 0
```

Workloads short of the Instances they want:

```promql
datum_cloud_compute_workload_status_replicas_desired{resourcemanager_datumapis_com_project_name="PROJECT_ID"}
  - datum_cloud_compute_workload_status_replicas_ready{resourcemanager_datumapis_com_project_name="PROJECT_ID"}
  > 0
```

How many of each kind exist:

```promql
count by (__name__) ({__name__=~"datum_cloud_.*_info|dns_.*_info", resourcemanager_datumapis_com_project_name="PROJECT_ID"})
```

A load balancer that has been unprogrammed for more than 15 minutes:

```promql
(
  datum_cloud_networking_http_proxy_status_condition{resourcemanager_datumapis_com_project_name="PROJECT_ID", condition="Programmed"} == 0
)
and on (resource_name, condition)
(
  time() - datum_cloud_networking_http_proxy_status_condition_last_transition_time{resourcemanager_datumapis_com_project_name="PROJECT_ID"} > 900
)
```

The same shape works for any kind that publishes `_last_transition_time`.

Join a label from `_info` onto a status series, for example the domain name:

```promql
(datum_cloud_networking_domain_status_condition{resourcemanager_datumapis_com_project_name="PROJECT_ID", condition="Verified"} == 0)
  * on (resource_name) group_left (domain_name)
datum_cloud_networking_domain_info{resourcemanager_datumapis_com_project_name="PROJECT_ID"}
```

## Reading a result

- A `_status_condition` value of `0` means the condition is false, or unknown.
  Check the `status` label (`False` or `Unknown`) and `reason` to tell them
  apart, then use the product's own triage, such as `datumctl alb describe`.
- A resource with no `_status_condition` series yet usually was just created,
  or its controller hasn't written status. Check `_info` to confirm it exists.
- Status series are sampled, so a condition that flips and recovers within a
  scrape can be missed. For "what happened when", use the activity log
  (milo-activity plugin) instead.
- Series stop when the resource is deleted. A resource that vanishes from
  `_info` was deleted, not fixed.

## Alerting

For an alert rule in Grafana Cloud, use a `== 0` query from above with
`for: 10m` or longer, since a condition is briefly false while a change rolls
out. Group by `resource_name` and `condition`, and put `reason` in the
notification.

## Guardrails

- Read-only. Querying metrics changes nothing.
- A metric name that returns nothing may have no matching resources, or may not
  be exported. Check the export policy's selector before concluding a resource
  is missing.
- Metric families can change while the APIs are alpha. If a family in the
  catalog returns nothing at all, search the destination for
  `{__name__=~"<resource>.*"}` to see what's there.
