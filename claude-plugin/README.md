# milo-observe

A Claude Code plugin for observing what's running in a Datum Cloud project. It
teaches Claude to read your project's logs, send your metrics to Grafana Cloud
or another Prometheus remote-write endpoint, and query the status metrics Datum
publishes for every resource.

| Skill | Use it to |
| --- | --- |
| `query-logs` | Search, tail, and filter project logs, including load balancer access logs, with `datumctl telemetry` and LogQL. |
| `export-metrics` | Create and check an `ExportPolicy` that ships project metrics to Grafana Cloud or any remote-write endpoint. |
| `status-metrics` | Find resources that aren't ready, count resources, and write alerts and dashboards from the `_info` and `_status_condition` series. |

Claude picks the skill from what you ask, for example "show me the 5xx
responses from the last hour", "send my metrics to Grafana Cloud", or "alert me
when a domain stops being verified".

## Install

```bash
claude plugin marketplace add datum-cloud/claude-code-plugins
claude plugin install milo-observe@datum-claude-code-plugins
```

## Prerequisites

- [`datumctl`](https://www.datum.net/docs/datumctl/install), signed in with
  `datumctl login` and pointed at a project with `datumctl ctx use`.
- For logs, the telemetry plugin: `datumctl plugin install telemetry`.
- For metrics in Grafana, a Grafana Cloud stack or another endpoint that accepts
  Prometheus remote write.

## Develop

The plugin lives next to the code it describes: the `datumctl telemetry` plugin
in [`cli/`](../cli/) and the `ExportPolicy` API in [`operator/`](../operator/).
When either changes, update the matching skill in the same pull request.

To try a local change, load the plugin from this folder:

```bash
claude --plugin-dir ./claude-plugin
```

Bump `version` in `.claude-plugin/plugin.json` for every change you want users
to receive. Users get an update only when that version changes.
