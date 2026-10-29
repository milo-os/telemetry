---
name: query-logs
description: Query a Datum Cloud project's logs with the datumctl telemetry plugin and LogQL. Use when someone asks to see, search, tail, or filter logs from their workloads, load balancers, or other Datum resources, or asks why a request failed.
---

# Query project logs

Datum keeps every log line from a project, including Compute workload output and
load balancer access logs, in the project's log store. The `datumctl telemetry`
plugin reads them with a LogQL stream selector.

## Before you start

1. Check the plugin is installed: `datumctl telemetry --help`. If datumctl says
   the command is unknown, install it with `datumctl plugin install telemetry`.
2. Check the active project: `datumctl ctx`. Logs are scoped to one project on
   the server. Pass `--project PROJECT_ID` to read another, or switch with
   `datumctl ctx use`.

## Find the labels to match on

Don't guess label names. List the ones the project's logs carry, then the values
of the one you need:

```bash
datumctl telemetry labels
datumctl telemetry labels service_name --since 24h
```

Both take `--since`, `--start`, and `--end`, and `-o json`.

## Query

```bash
# The newest 100 lines from the last hour, oldest first
datumctl telemetry logs '{service_name="checkout"}'

# Lines containing "error" over the last day, every line in the window
datumctl telemetry logs '{service_name="checkout"} |= "error"' --since 24h --limit -1

# An explicit window
datumctl telemetry logs '{service_name="checkout"}' --start 2026-10-06T09:00:00Z --end 2026-10-06T10:00:00Z

# Keep printing new lines
datumctl telemetry logs '{service_name="checkout"}' -f

# One JSON object per line, for scripts
datumctl telemetry logs '{service_name="checkout"}' -o json | jq .line
```

| Flag | Meaning |
| --- | --- |
| `--since` | Only lines newer than this, such as `15m`, `2h`, `7d`. Default `1h`. |
| `--start`, `--end` | An explicit window, as RFC 3339 or Unix seconds. `--end` defaults to now. |
| `--limit` | Newest lines to show. Default `100`; `-1` shows every line in the window. |
| `-f`, `--follow` | Keep polling for new lines. Can't be combined with `--end`. |
| `--labels` | Prefix each line with its label set. |
| `--utc` | Show timestamps in UTC. |
| `-o json` | One JSON object per line. |

## What the query language supports

The plugin accepts a subset of LogQL:

- A stream selector with **at least one** label matcher: `=`, `!=`, `=~`, `!~`.
- Optional line filters after it: `|=` (contains), `!=` (doesn't contain),
  `|~` (matches regex), `!~` (doesn't match regex). Chain as many as you need:
  `{service_name="checkout"} |= "error" != "healthz"`.

It does **not** support parsers (`| json`, `| logfmt`), formatting stages, or
metric queries (`rate(...)`, `count_over_time(...)`). To count or graph, export
metrics instead (see the `export-metrics` skill), or fetch lines with `-o json`
and count them locally.

## Common queries

**Load balancer access logs.** Each access log line carries labels such as
`authority` (the hostname), `method`, `path`, `response_code`,
`response_flags`, `duration`, `upstream_host`, `user_agent`, `request_id`, and
`zone_name` (the Datum location that served it).

```bash
# Every request any load balancer in the project answered with 403
datumctl telemetry logs '{response_code="403"}' --since 1h

# 5xx responses for one hostname
datumctl telemetry logs '{authority="api.example.com", response_code=~"5.."}' --since 1h
```

For a single load balancer, `datumctl alb logs ALB_NAME` gives a table with
`--code`, `--method`, `--host`, and `-o wide` (adds the request ID and
upstream). An upstream of `-` means Envoy answered without reaching the origin,
for example when the firewall blocked the request.

**Workload output.** Run `datumctl telemetry labels` to find the workload and
stream labels on your project, then select on them, for example:

```bash
datumctl telemetry logs '{datum_workload_name="api", log_iostream="stderr"} |= "panic"' --since 6h
```

## Guardrails

- Read-only. Querying logs changes nothing.
- Lines can take a few minutes to appear after they're written. With `-f`, new
  lines arrive up to a couple of seconds late, because the plugin polls.
- Start narrow. A bare `{response_code=~".+"}` over 7 days with `--limit -1` can
  return a very large result. Narrow the window or add a filter first.
- If a query returns nothing, check the label and value with
  `datumctl telemetry labels NAME` for the same window before concluding there
  were no logs.
- When an error prints a hint, follow it. A permission error means the signed-in
  user can't read logs in that project.
