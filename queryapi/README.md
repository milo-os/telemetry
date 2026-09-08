# queryapi

Tenant-scoped log and metric query API over ClickHouse, serving `datumctl` and
the customer cloud portal. See [`openapi.yaml`](openapi.yaml) for the contract.

queryapi is a Kubernetes **aggregated API server** -- it runs on
`k8s.io/apiserver`'s `GenericAPIServer`, the way
[activity](https://github.com/datum-cloud/activity) does -- but it is not a
CRUD resource server. It stores nothing in etcd (all of its data is in
ClickHouse), registers no `rest.Storage`, and serves Loki- and
Prometheus-shaped routes. What it takes from the framework is the part that
has to be identical everywhere: TLS, the delegating authenticator, the
delegating authorizer, health, metrics, and the discovery document
kube-aggregator probes before it will proxy anything.

## Paths

kube-aggregator forwards the request path verbatim, so production sends
`/apis/o11y.miloapis.com/v1alpha1/...`. Two shorter forms are normalised onto
it before anything else looks at the request:

| Form | Example |
| --- | --- |
| aggregator-forwarded (canonical) | `/apis/o11y.miloapis.com/v1alpha1/logs/loki/api/v1/query` |
| Grafana's Loki datasource | `/loki/api/v1/query` |
| compatibility | `/v1alpha1/logs/loki/api/v1/query` |

The rewrite is the outermost filter in the chain, so the path that is
authorized is the path that is served. Segments are matched whole and only at
the head: a `/loki/api/v1` or `/projects/...` lookalike further along is a
label name, and survives untouched.

`/apis/o11y.miloapis.com/v1alpha1` itself serves the discovery document.
kube-aggregator's availability controller probes it and marks the APIService
unavailable on anything that is not a 2xx or 3xx, and an unavailable APIService
is never proxied to -- so the document is what makes every other path
reachable. It lists no resources, honestly: `logs` and `metrics` name
permissions, not collections.

## Storage backends

| `--storage` | Behaviour |
| --- | --- |
| `fake` (default) | Deterministic synthetic logs, a pure function of the query's time range. Same range always returns the same rows. |
| `clickhouse` | Serves log queries and label/stream discovery against the `o11y.logs` schema via LogQL-to-SQL translation. `/readyz` pings the backend. |

Metric (Prometheus-shaped) endpoints return 501 on both backends.

## Flags

Serving, authentication and authorization come from `k8s.io/apiserver`'s
`RecommendedOptions`, so those flags are the ones every Kubernetes API server
takes and they mean the same things here. `--help` lists all of them; these are
the ones that matter.

The table below is what the *binary* defaults to. In the cluster none of these
values is written as a literal: every flag the deployment sets is spelled
`--flag=$(ENV_VAR)` against an env var, so that a consumer can override one of
them without restating the rest. See [Deployment](#deployment).

| Flag | Default |
| --- | --- |
| `--secure-port` | `8443` (HTTPS only; it cannot be turned off) |
| `--bind-address` | `0.0.0.0` |
| `--tls-cert-file`, `--tls-private-key-file` | none -- self-signs into `--cert-dir` |
| `--cert-dir` | `apiserver.local.config/certificates` |
| `--authentication-kubeconfig` | in-cluster config -- see [Where the reviews go](#where-the-reviews-go) |
| `--requestheader-client-ca-file` | empty: read the CA from `extension-apiserver-authentication` in `kube-system` |
| `--authorization-kubeconfig` | in-cluster config -- see [Where the reviews go](#where-the-reviews-go) |
| `--authorization-always-allow-paths` | `/healthz,/readyz,/livez,/metrics` |
| `--authorization-webhook-cache-authorized-ttl` | `10s` |
| `--authorization-webhook-cache-unauthorized-ttl` | `10s` |
| `--storage` | `fake` |
| `--fake-rate` | `2` (lines/sec) |
| `--query-timeout` | `30s` |
| `--default-limit` | `100` |
| `--max-limit` | `5000` |

There is deliberately no flag that lets a client name its own project, and no
flag that turns authentication or authorization off. Every one of them would be
a way to read another tenant's telemetry. `--authorization-always-allow-paths`
is the one flag that removes a gate, and it is checked at startup: a value that
names `/apis/o11y.miloapis.com` refuses to start.

Some `RecommendedOptions` flags are absent because the corresponding option is
nil, and each absence is a decision:

| Absent | Why |
| --- | --- |
| `--etcd-*` | queryapi persists nothing; its data is in ClickHouse. |
| `--enable-admission-plugins` and friends | It admits nothing -- there are no writes. |
| `--kubeconfig` (`CoreAPIOptions`) | Nothing reads the core API. The clients authentication and authorization need are built from their own kubeconfig flags. |
| `--profiling`, defaulted off | Nothing here needs pprof, and enabling it would also swap the read-only `/metrics` handler for the one that resets the registry on `DELETE` -- on a path left unauthorized. |
| `--enable-priority-and-fairness`, defaulted off | It needs a core API client and shared informers to read `FlowSchema`s with. The plain in-flight limiter stands in. |

The read-header and idle timeouts are not configurable: the apiserver's secure
serving stack owns the `http.Server` and sets them (32s and 90s).

## ClickHouse environment

Used only with `--storage=clickhouse`. Authentication is by client
certificate, not password, matching `clickhouse-migrate`.

| Env var | Default |
| --- | --- |
| `CLICKHOUSE_HOST` | required |
| `CLICKHOUSE_PORT` | `9440` |
| `CLICKHOUSE_USER` | `queryapi` |
| `CLICKHOUSE_DATABASE` | `o11y` |
| `CLICKHOUSE_TLS_CERT_FILE` | `/etc/clickhouse-client/certs/tls.crt` |
| `CLICKHOUSE_TLS_KEY_FILE` | `/etc/clickhouse-client/certs/tls.key` |
| `CLICKHOUSE_TLS_CA_FILE` | `/etc/clickhouse-client/certs/ca.crt` |

## Tenancy and authorization

Every request is authenticated by the framework, then authorized by Milo. Both
gates are Kubernetes APIs -- nothing here is a bespoke trust decision.

### Identity

queryapi is reached only through Milo's aggregator/proxy chain. That chain
already authenticates the request, scopes it to a project, and forwards the
authenticated identity to us as `X-Remote-*` headers.

Those headers are turned back into a `user.Info` by the Kubernetes **delegating
authenticator**, which is not optional and has no reduced mode:

- queryapi terminates TLS itself, so every connection can carry a client
  certificate;
- the certificate is verified against the front proxy's CA, read by default
  from the `extension-apiserver-authentication` ConfigMap in `kube-system` of
  whichever cluster `--authentication-kubeconfig` selects (hence the RoleBinding
  in [`config/queryapi/rbac.yaml`](../config/queryapi/rbac.yaml)), or from
  `--requestheader-client-ca-file` when that names one;
- only then are the `X-Remote-*` headers on that connection believed;
- a caller presenting a bearer token directly is authenticated by `TokenReview`
  instead.

Nothing a client sets for itself -- no header, no path segment, no query
parameter -- names the project. A caller the authenticator cannot identify at
all is a 401. A caller it can identify but that is acting outside a project is
a 403: the delegating authenticator admits `system:anonymous` rather than
rejecting it, which is how the kubelet reaches `/healthz`, so "unidentified"
and "unauthorized" both land on the authorization filter.

### Authorization

Authorization is delegated to Milo, exactly as the activity service delegates
it: the framework's `DelegatingAuthorizationOptions` posts a
`SubjectAccessReview` to the apiserver `--authorization-kubeconfig` points at.
queryapi writes no review code and makes no tenancy decision of its own.

Tenancy rides in the review. The caller's `iam.miloapis.com/parent-type` and
`iam.miloapis.com/parent-name` extras survive authentication into
`SubjectAccessReviewSpec.Extra`, and Milo evaluates the permission against the
project they name. There is no project field in `ResourceAttributes` to set,
and nothing for queryapi to get wrong.

What queryapi supplies is the vocabulary. A custom `RequestInfoResolver` maps
each route onto one Milo permission:

| Route (under `/apis/o11y.miloapis.com/v1alpha1`) | Permission |
| --- | --- |
| `/logs/loki/api/v1/query`, `.../query_range` | `o11y.miloapis.com/logs.query` |
| `/logs/loki/api/v1/labels`, `.../label/{name}/values` | `o11y.miloapis.com/logs.getLabels` |
| `/logs/loki/api/v1/series` | `o11y.miloapis.com/logs.getSeries` |
| `/metrics/api/v1/query`, `.../query_range` | `o11y.miloapis.com/metrics.query` |
| `/metrics/api/v1/labels`, `.../label/{name}/values` | `o11y.miloapis.com/metrics.getLabels` |
| `/metrics/api/v1/series` | `o11y.miloapis.com/metrics.getSeries` |

`query` returns log lines or samples; the `get*` actions return only metadata
about them, which is a separate boundary because label values carry pod names,
hostnames and customer identifiers. All six are granted by the
`telemetry.miloapis.com-viewer` role
([`config/operator/iam/`](../config/operator/iam/)). The metrics routes return
501 today and are gated anyway, so they cannot ship unguarded.

`queryapi_authorization_total{resource,verb,decision}` reports the outcomes.

### Where the reviews go

Both delegating options default to in-cluster config, which means the API
server queryapi is running next to, reached with its mounted service account
token. That is correct when queryapi runs **inside the control plane** that
authenticates callers and evaluates IAM.

It is wrong, quietly, when queryapi runs somewhere else -- beside its storage,
in a cluster the aggregator only proxies into. Then:

- `SubjectAccessReview`s go to the local API server, which knows none of the
  permissions in `internal/authz/permissions.go` and denies every query;
- the front proxy's CA is read from the local
  `extension-apiserver-authentication`, which holds that cluster's front proxy
  rather than the one whose client certificate is actually arriving.

Neither failure announces itself as a misconfiguration: the first is a 403 that
looks like a missing role, the second a 401 that looks like a bad certificate.
A deployment in that shape must set `--authentication-kubeconfig` and
`--authorization-kubeconfig` to a kubeconfig for the control plane, with a
credential that may create `SubjectAccessReview`s there, and point
`--requestheader-client-ca-file` at that control plane's CA -- which also
retires the `kube-system` RoleBinding, since naming the file skips the ConfigMap
lookup. Confirm with one `SubjectAccessReview` by hand before trusting an allow.

### Why an unmapped path cannot be served

Two independent reasons, either of which would be sufficient:

1. **The resolver only ever describes a mapped route as a resource request.**
   It matches with the tenant mux itself -- the same matcher, on the same
   path -- so the permission reviewed always belongs to the handler that will
   run. Anything else under `/apis/o11y.miloapis.com` becomes a *non-resource*
   request, which reaches no telemetry handler: the tenant mux serves nothing
   there and answers 404.
2. **`authz.Guard` denies it before the review.** Under this service's group
   only two things are forwarded to Milo: a discovery document, and a resource
   request naming a permission in the closed vocabulary above. An endpoint
   declared in `openapi.yaml` but never registered -- `/loki/api/v1/tail`, say
   -- is denied outright rather than asked about as a non-resource URL that
   Milo might grant broadly.

`--authorization-always-allow-paths` cannot open a query route either: those
are resource requests by the time they are authorized, and the path authorizer
abstains on every resource request. Startup validation rejects such a value
anyway.

### The `system:masters` bypass

`DelegatingAuthorizationOptions` sets `AlwaysAllowGroups: ["system:masters"]`,
and queryapi keeps it. This is load-bearing, not incidental:
kube-aggregator's availability controller probes
`/apis/o11y.miloapis.com/v1alpha1` as `system:kube-aggregator` in
`system:masters`, and without the bypass that probe would need a Milo grant
before the APIService could ever be available.

The cost is stated plainly: **anything the front proxy presents to queryapi as
`system:masters` reads any project's telemetry without a review.** The front
proxy's client certificate is what limits who can make that claim. Activity
accepts the same bargain, as does every aggregated API server built on
`RecommendedOptions`.

### Defence in depth

Authorization is a gate above the existing ones, not a replacement. The project
Milo evaluated the review against and the project bound to ClickHouse's
`telemetry_project_id` setting come from one `miloauth.Resolve` on one verified
`user.Info`, so they cannot diverge; the ClickHouse row policy and the storage
layer's own project check both stay.
[`config/queryapi/networkpolicy.yaml`](../config/queryapi/networkpolicy.yaml)
keeps unauthenticated traffic off the listener entirely. It is no longer what
makes the forwarded identity trustworthy -- the client-CA check is -- but it is
still worth having, and it is also what limits who can assert
`system:masters`.

## Deployment

[`config/queryapi/`](../config/queryapi/) is the bundle. It is deliberately not
self-sufficient: it names three things the consumer owns, and applying it
without patching them will not work. A consumer running queryapi outside the
control plane has a fourth thing to supply, which is not a placeholder because
the defaults are valid values rather than obviously empty ones -- see
[Where the reviews go](#where-the-reviews-go).

| PLACEHOLDER | Where | What to supply |
| --- | --- | --- |
| `PLACEHOLDER-issuer` | `deployment.yaml`, `csi.cert-manager.io/issuer-name` | The `ClusterIssuer` that signs the serving certificate. In `datum-cloud/infra` this is the `o11y-system` one. |
| `PLACEHOLDER-milo-namespace` | `networkpolicy.yaml` | The namespace running `milo-apiserver`. |
| `PLACEHOLDER-issuer-ca` | [`config/queryapi-api-registration/apiservice.yaml`](../config/queryapi-api-registration/apiservice.yaml), `cert-manager.io/inject-ca-from-secret` | The `Secret` holding that issuer's CA, so cainjector can fill the APIService's `caBundle`. |

### The serving certificate

TLS is not optional here -- only a TLS connection carries the front proxy's
client certificate, and only that certificate makes the forwarded `X-Remote-*`
headers worth believing -- but no `Certificate` object and no long-lived
`Secret` full of private key exist for it. The pair comes from the
**cert-manager CSI driver**, the same way the collectors get their NATS
certificates: an ephemeral volume, one certificate per pod, issued at mount
time and renewed in place.

```yaml
- name: serving-cert
  csi:
    driver: csi.cert-manager.io
    volumeAttributes:
      csi.cert-manager.io/issuer-name: PLACEHOLDER-issuer
      csi.cert-manager.io/issuer-kind: ClusterIssuer
      csi.cert-manager.io/dns-names: "queryapi.o11y-system.svc,queryapi.o11y-system.svc.cluster.local"
      csi.cert-manager.io/key-usages: "server auth"
```

`server auth` and the `dnsNames` are what make it a *serving* certificate: it
has to name the addresses the aggregator dials. `o11y-system` appears there,
in `apiservice.yaml`, and in `rbac.yaml`'s subjects; a consumer deploying
elsewhere patches all three together.

The consequence for the aggregator is that there is no `Certificate` to point
`cert-manager.io/inject-ca-from` at any more. The APIService names the issuing
CA's `Secret` instead (`cert-manager.io/inject-ca-from-secret`), which is the
thing the aggregator actually has to trust; that `Secret` needs
`cert-manager.io/allow-direct-injection: "true"` on it before cainjector will
read it. `insecureSkipTLSVerify` stays `false`. `apiservice.yaml` documents the
two alternatives -- patching `caBundle` by hand, or giving up backend
verification -- and when each is defensible.

### Overriding a flag

`args` is a list, and a strategic merge patch replaces a list wholesale: a
consumer overriding one flag would have to restate every other one, and would
silently drop any flag added later. So no flag carries a literal. `env` merges
by name, so one entry can be patched on its own:

```yaml
patches:
  - patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: queryapi
      spec:
        template:
          spec:
            containers:
              - name: queryapi
                env:
                  - name: STORAGE
                    value: clickhouse
```

That changes exactly one line of the rendered output. The one flag this does
not fully cover is `SECURE_PORT`: the `containerPort` and the Service's
`targetPort` name the same listener, so patch all three or none.

`CLICKHOUSE_*` (see [ClickHouse environment](#clickhouse-environment)) is read
from the environment directly rather than through a flag, so it is already
patchable the same way.

### e2e and throwaway clusters

[`config/queryapi-e2e/`](../config/queryapi-e2e/) is an overlay over
`config/queryapi` for a cluster that has none of the above. It adds a
self-signed CA, repoints the CSI volume at it (as a namespaced `Issuer`),
repoints the APIService's CA injection at that CA's `Secret`, and opens the
NetworkPolicy. `kustomize build config/queryapi-e2e` applies to an empty
cluster and works.

The CA is bootstrapped -- a selfSigned `Issuer` issues a CA `Certificate`,
which backs a `ca` `Issuer` -- rather than being a selfSigned issuer used
directly, because a selfSigned issuer leaves no CA object behind for the
aggregator to trust. Going through a real CA means e2e verifies the backend
exactly as production does, instead of setting `insecureSkipTLSVerify: true`
and testing a path nobody ships. It is not for production: the CA is generated
in-cluster and trusted by nothing else.

## Local development

```sh
task queryapi:test
task queryapi:lint
```

Running the server needs a cluster, because both gates are Kubernetes APIs:
authentication reads the front proxy's CA out of `kube-system`, and
authorization posts a `SubjectAccessReview`. This is how the activity service is
run too, and there is no bypass flag -- one used to exist and was removed,
because a caller that names its own project has already given up the boundary
the review defends.

```sh
go run ./cmd \
  --secure-port=8443 \
  --authentication-kubeconfig="$KUBECONFIG" \
  --authorization-kubeconfig="$KUBECONFIG"
```

With no `--tls-cert-file` the process self-signs a serving certificate into
`--cert-dir`, so clients need `-k`. Requests must arrive over TLS carrying a
client certificate signed by the CA in that cluster's
`extension-apiserver-authentication` ConfigMap -- on kind, the control-plane
node's `/etc/kubernetes/pki/front-proxy-ca.{crt,key}`:

```sh
curl -k --cert front-proxy-client.crt --key front-proxy-client.key \
  -H 'X-Remote-User: you@example.com' \
  -H 'X-Remote-Extra-Iam.miloapis.com%2Fparent-type: Project' \
  -H 'X-Remote-Extra-Iam.miloapis.com%2Fparent-name: proj-abc' \
  'https://localhost:8443/loki/api/v1/labels'
```

Without that certificate the same request authenticates as `system:anonymous`
and is denied, which is the point. To reproduce the availability probe:

```sh
curl -k --cert front-proxy-client.crt --key front-proxy-client.key \
  -H 'X-Remote-User: system:kube-aggregator' \
  -H 'X-Remote-Group: system:masters' \
  https://localhost:8443/apis/o11y.miloapis.com/v1alpha1
```

`/healthz`, `/livez`, `/readyz` and `/metrics` are left unauthorized and answer
over plain `curl -k`.
