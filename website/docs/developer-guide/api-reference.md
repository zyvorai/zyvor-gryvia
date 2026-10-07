# API Reference

REST API of the Gryvia API gateway (`services/api-gateway`, FastAPI). It is the API the web dashboard uses. The
authoritative list is the route code in `services/api-gateway/main.py` and `services/api-gateway/routers/*.py`; when
the gateway is reachable, `GET /docs` serves the generated OpenAPI (Swagger) page.

Everything here is served under `/api/`. There is no `/api/v1` prefix, and there are no webhook, WebSocket or
Prometheus (`/metrics`) endpoints on the gateway. The CLI does not use this API: it talks to the Kubernetes API with
your kubeconfig. The Python SDK is a client of this API; the Go SDK is a Kubernetes client (see [SDKs](#sdks)).

## Base URL

Inside the cluster the gateway serves HTTPS with the chart's certificate (self-signed by default, so clients must trust
it or skip verification):

```
https://gryvia-api-gateway.gryvia-system.svc.cluster.local:8080
```

Through the dashboard (`gryvia-ui`, port 443, or the NodePort/Ingress you configured) the same routes are proxied under
`/api/`:

```
https://<dashboard-host>/api/...
```

`https://<server-ip>:32443` is the NodePort that `scripts/deploy-remote.sh` uses.

## Authentication and roles

Every route except `/`, `/health`, `/api/auth/config` and `/api/auth/login` needs `Authorization: Bearer <token>`.
The token is one of:

| Token | Role | How you get it |
|-------|------|----------------|
| The API key (`GRYVIA_API_KEY`, chart `auth.apiKey`) | **admin** | You set it. The default lab key `Admin@321` is well known; change it. |
| A signed session (`gs1.` prefix, 8 hours by default) | **admin** | `POST /api/auth/login` with `{"username": "admin", "password": "<api key>"}` |
| An OIDC JWT (when `OIDC_ENABLED=true`) | **tenant** (or **admin** if its `groups` contain a value of `GRYVIA_OIDC_ADMIN_GROUPS`) | Your identity provider |

```bash
curl -k -H "Authorization: Bearer $GRYVIA_API_KEY" https://<dashboard-host>/api/cluster/stats
```

A tenant user's token must map to a `GryviaTenant` (by `org` claim, or by `groups`); otherwise every call is 403. The
request is then confined to that tenant's namespace(s) (`tenant-<name>`). See
[Authentication and TLS](../guides/AUTH_AND_TLS.md) and [GPU as a Service](../guides/GPU_AS_A_SERVICE.md).

Status codes: `401` no or expired credentials, `403` wrong credentials, a tenant calling an admin-only route, or an
unmapped OIDC user, `404` not found (also used for another tenant's object), `429` rate limit, `503` a backing source
(collector, Flight Recorder token) is unavailable. Errors have the FastAPI shape `{"detail": "..."}`.

Rate limits are per client address and per route (for example `30/minute` on reads, `10/minute` on writes and sign-in,
`60/minute` on job logs and events, `10/minute` on `/api/usage/export`). The dashboard proxy forwards the client address.

In the tables below **Access** means:

- **open**: no token needed.
- **any**: any authenticated caller. Lists are filtered to the caller's namespaces (**scoped**): a tenant user sees only
  its tenant's namespaces, the admin sees the gateway's job namespace (`GRYVIA_JOB_NAMESPACE`; the chart sets it to
  the release namespace, `gryvia-system`; it is `default` when unset).
- **admin**: the API key, a session or an OIDC admin group; tenants get 403.
- **tenant-filtered**: any caller, but the content is filtered by tenant as described.

## Health and auth

| Method and path | Access | Purpose |
|---|---|---|
| `GET /` | open | `{"status": "healthy", "service": "gryvia-api-gateway"}` |
| `GET /health` | open | `{"status": "ok"}` (liveness) |
| `GET /api/auth/config` | open | Which sign-in methods are on (`oidcEnabled`, `apiKeyEnabled`), the OIDC endpoints the dashboard needs, `instance` (`product`, `version`, `namespace`) and, when API-key sign-in is on, `credentials` (`username`, `secret`, `key`, `namespace`): where the admin key is stored, never the key |
| `POST /api/auth/login` | open | Exchange `admin` and the API key for a session token; returns `token`, `expiresAt`, `usingDefaultKey`. Rate limited (10/minute) and delayed on failure |
| `GET /api/auth/me` | any | Caller identity: `method`, `role`, `tenant`, `tenants`, `tenantNamespaces`, plus OIDC claims. `usingDefaultKey` for API-key callers |

## Cluster, metrics and nodes

| Method and path | Access | Purpose |
|---|---|---|
| `GET /api/cluster/stats` | any (scoped) | Job counts for the caller's namespaces; GPU and node capacity only for the admin (tenants get zeros) |
| `GET /api/metrics/gpu?time_range=1h` | admin | GPU utilisation, temperature and memory from Prometheus/DCGM; `time_range` is one of `1h`, `6h`, `24h`, `7d`, `30d` |
| `GET /api/metrics/costs` | any (scoped) | Spend to date by team and GPU type. Uses `GryviaUsageRecord`s when any exist, otherwise computes from job run time priced from the SKU catalog. `hasHistoricalData` says whether Prometheus history was available |
| `GET /api/metrics/jobs?time_range=24h` | any (scoped) | Job counts by status and framework, average duration |
| `GET /api/nodes` | admin | `GryviaGpuNode` objects (`?limit=500&offset=0`) |
| `GET /api/nodes/health` | admin | Per-node GPU health summary (`?limit=100&offset=0`) |
| `GET /api/nodes/{name}` | admin | One node |
| `GET /api/quotas`, `GET /api/quotas/{name}` | any (scoped) | `GryviaQuota`s; a tenant sees quotas whose `spec.namespaces` intersect its namespaces |
| `GET /api/quota/usage` | any (scoped) | Quota usage per team, same visibility rule |

## Jobs

| Method and path | Access | Purpose |
|---|---|---|
| `GET /api/jobs` | any (scoped) | `GryviaAIJob`s (`?limit=500&offset=0`, limit up to 1000) |
| `POST /api/jobs` | any (scoped) | Create a job. The body must be a `GryviaAIJob` with `apiVersion: gryvia.io/v1alpha1`, `spec.image` and `spec.gpus`; the gateway sets the namespace itself (the caller's first namespace). 10/minute |
| `GET /api/jobs/{name}` | any (scoped) | One job |
| `DELETE /api/jobs/{name}` | any (scoped) | Delete a job. 10/minute |
| `GET /api/jobs/{name}/pods` | any (scoped) | Pods labelled `gryvia.io/job=<name>` |
| `GET /api/jobs/{name}/logs?pod=&tail=200` | any (scoped) | JSON `{pod, container, lines, truncated}`; `tail` 1 to 2000. It is a snapshot, not a stream |
| `GET /api/jobs/{name}/events` | any (scoped) | Kubernetes events for the job and its pods |

The log and event routes need the gateway service account to read `pods/log` and `events`; the chart grants that.

## ML workflow objects (scoped)

These routes create, list and delete the corresponding custom resources in the caller's namespace. The ai-operator's ML
controllers (on by default, `--enable-ml-controllers`) run them: workspaces and inference services become pods,
Deployments and Services, workflows and tuners create child jobs, and registry entries drive promotion and serving. See
[ML controllers](https://github.com/zyvorai/gryvia/blob/main/docs/ml-controllers.md).

| Method and path | Access | Purpose |
|---|---|---|
| `GET/POST /api/workspaces`, `GET/DELETE /api/workspaces/{name}` | any (scoped) | Workspaces |
| `POST /api/workspaces/{name}/pause`, `POST /api/workspaces/{name}/resume` | any (scoped) | Toggle a workspace |
| `GET /api/models`, `GET /api/models/{name}` | any (scoped) | Model registry entries |
| `POST /api/models/{name}/promote` | any (scoped) | Move a model to the next stage (`{"targetStage": "..."}`); 409 for an invalid transition |
| `GET/POST /api/inference`, `GET/DELETE /api/inference/{name}` | any (scoped) | Inference services |
| `GET/POST /api/workflows`, `GET/DELETE /api/workflows/{name}` | any (scoped) | Workflows |
| `GET/POST /api/tuners`, `GET/DELETE /api/tuners/{name}`, `GET /api/tuners/{name}/trials` | any (scoped) | Auto tuners and their trials |
| `GET/POST /api/model-watches`, `GET/DELETE /api/model-watches/{name}`, `GET /api/model-watches/{name}/runs`, `POST /api/model-watches/{name}/suspend`, `POST /api/model-watches/{name}/resume` | any (scoped) | Model watches (model factory) and the models each one found |

## GPU as a Service

| Method and path | Access | Purpose |
|---|---|---|
| `GET /api/skus`, `GET /api/skus/{name}` | tenant-filtered | The catalog. A tenant sees enabled SKUs, limited to the `allowedSkus` of its tenant(s) |
| `POST /api/skus`, `PUT /api/skus/{name}`, `DELETE /api/skus/{name}` | admin | Manage SKUs (`gpuType`, `gpusPerUnit`, `hourlyRate`, `currency`, `spotDiscount`, `description`, `enabled`) |
| `GET /api/experiments`, `GET /api/experiments/{name}` | tenant-filtered | `GryviaLiveExperiment`: spec summary and the leaderboard re-ranked by the experiment's direction, with `deltaToBest` / `deltaPct` per run. Read-only |
| `GET /api/federations`, `GET /api/federations/{name}` | admin | Read-only `GryviaFederation` view: per-cluster state and utilisation, aggregate GPU totals (declared capacity). Never returns the API server address or credentials secret name, only `hasCredentials` |
| `GET /api/tenants`, `GET /api/tenants/{name}` | tenant-filtered | The admin sees every tenant, a tenant user only its own |
| `GET /api/audit` | admin | Recent state-changing requests (newest first; `limit`, `outcome`, `method`). Per-replica buffer lost on restart; the `gryvia.audit` log is the durable record |
| `POST /api/tenants`, `DELETE /api/tenants/{name}` | admin | Create (`name`, `displayName`, `allowedSkus`, `maxGPUs`, `isolated`) or delete a tenant |
| `GET /api/usage?tenant=&from=&to=&groupBy=tenant\|sku\|day` | tenant-filtered | Metered GPU hours and cost from `GryviaUsageRecord`s. A tenant user is always limited to its own namespaces, whatever `tenant` says |
| `GET /api/usage/export?format=csv\|json&tenant=&from=&to=` | tenant-filtered | Per-record export as an attachment |
| `GET /api/invoices?month=YYYY-MM&tenant=` | tenant-filtered | Estimate invoices, one per tenant with usage in the month (default: current UTC month) |
| `GET /api/invoices/{tenant}/{YYYY-MM}?format=json\|csv` | tenant-filtered | One invoice; 404 when the tenant has no usage that month (or is not yours) |

Usage and invoices are **estimates** computed from job wall-clock time and the SKU rate. There is no payment
processing, tax handling or capacity reservation.

## Network, security and GPU analysis (admin only)

| Method and path | Purpose |
|---|---|
| `GET /api/network/flows` | Real flows from [Netra](https://github.com/zyvorai/netra) when `apiGateway.netra.url` is set and reachable (`"source": "netra"`); otherwise the edges of `GryviaServiceGraph` objects |
| `GET/POST /api/network/policies`, `POST /api/network/policies/{name}/apply` | `GryviaFlowPolicy` objects; `apply` annotates one (`gryvia.io/apply-requested-at`) so the operator reconciles it again |
| `GET /api/network/insights`, `/graph`, `/anomalies`, `/costs` | Summaries built from `GryviaServiceGraph`, `GryviaFlowPolicy`, `GryviaNetworkAnomaly`, `GryviaTrafficInsight`, `GryviaTraceSession` and `GryviaNetworkCost` objects |
| `GET/POST /api/network/traces`, `GET /api/network/traces/{name}` | `GryviaTraceSession` objects |
| `GET /api/security/alerts` | Always `{"items": [], "eventSource": false}`: no per-event alert source is wired |
| `GET/POST /api/security/policies` | `GryviaSecurityPolicy` objects |
| `GET /api/ai/training/insight` | Latest `GryviaTrainingInsight` analysis (an empty shape when none) |
| `GET /api/ai/training/nccl` | NCCL per-operation stats merged from the collectors in `GRYVIA_COLLECTOR_URLS`; empty when none is reachable |
| `GET /api/gpu/memory` | Host/device transfer counters merged from the collectors; zeros when none is reachable |

These need the network-intelligence operator, and for the last three the eBPF collector, which is experimental and off
by default (see [Network Intelligence](../guides/NETWORK_INTELLIGENCE.md)).

## Flight Recorder (preview)

| Method and path | Access | Purpose |
|---|---|---|
| `GET /api/flight/jobs/{job}?namespace=<ns>` | tenant-filtered | Merges the node-local Flight Recorder timelines of every Running collector pod for a job. The admin may name any namespace, a tenant user only its tenant namespaces (403 otherwise). 503 when the Flight Recorder token (`apiGateway.flightTokenSecret`) is not configured or no collector is reachable |

The response carries `coverage` (`total`, `reachable`, `reporting`, `complete`), a `truncated` flag, `findings`,
`rankObservations` and the latest events. It reports observed events only. Details and limits:
[docs/flight-recorder.md](https://github.com/zyvorai/gryvia/blob/main/docs/flight-recorder.md). Unit-tested; not run on a
real cluster with GPUs.

## Workload intelligence

Analysis endpoints take **caller-supplied** data, store nothing and never apply changes; request bodies are capped at
512 KiB. Live job reads use the same namespace and marking filtering as the job routes. Details, limits and the
operation lifecycle: [workload intelligence](https://github.com/zyvorai/gryvia/blob/main/docs/workload-intelligence.md).

| Method and path | Access | Purpose |
|---|---|---|
| `GET /api/intelligence/capabilities`, `GET /api/intelligence/schemas` | any | Analysis areas, whether operations are enabled, limits, and the JSON schema of each analysis input |
| `POST /api/intelligence/{area}` | any | One analysis over the supplied input; `area` is `preflight`, `training`, `economics`, `serving`, `laboratory`, `recovery`, `locality`, `fabric`, `capacity` or `federation` |
| `GET /api/intelligence/jobs/{name}/explain` | any (scoped) | Job conditions and real pod scheduling failures |
| `GET /api/intelligence/jobs/{name}/economics` | any (scoped) | Usage-record cost estimate joined by job UID; rejects mixed currencies |
| `GET /api/intelligence/inventory` | admin | Registered GPU capacity; free capacity is reported as unknown |
| `GET/POST /api/intelligence/actions`, `GET /api/intelligence/actions/{id}` | admin, OIDC only | List, create and inspect operation proposals. 503 unless `apiGateway.intelligenceActions` is on |
| `POST /api/intelligence/actions/{id}/{approve,reject,execute,rollback}` | admin, OIDC only | Lifecycle transition; the proposer cannot review, execute or roll back their own operation |

Operations cover inference replicas, `GryviaQuota` GPU limits and node cordon only. Shared API keys and dashboard key
sessions cannot propose or review them.

## Collector endpoints (not on the gateway)

The eBPF collector (`collector/`, off by default) has its own HTTP listener on `:9090`: `/metrics`, `/healthz`,
`/api/v1/graph`, `/api/v1/anomalies`, `/api/v1/gpu/nccl`, `/api/v1/gpu/memory`, `/api/v1/fabric`,
`/api/v1/security/alerts`, `/api/v1/ai/training`, `/api/v1/ai/pipeline`, `/api/v1/tuning/tcp`, `/api/v1/ebpf/status`
and `/api/v1/flight/diagnose`. Only `/api/v1/flight/diagnose` authenticates its callers (an HMAC token shared with the
gateway); the rest is served without authentication, so keep the collector reachable only by cluster operators.

## SDKs

- **Python** (`sdk/python`): an async REST client of this API for jobs, nodes, quotas, costs and metrics. It does not
  cover SKUs, tenants, usage, invoices or the Flight Recorder. `client.intelligence` wraps the workload intelligence
  routes. Install from source (`pip install -e sdk/python`).
  See its [README](https://github.com/zyvorai/gryvia/blob/main/sdk/python/README.md).
- **Go** (`sdk/go`): a controller-runtime Kubernetes client for the Gryvia custom resources. It does not call this REST
  API.

## Configuration

Gateway environment variables (chart values in parentheses): `GRYVIA_API_KEY` (`auth.apiKey`),
`GRYVIA_API_KEY_SECRET` (the Secret holding it, `auth.existingSecret` or `gryvia-api-key`; shown on the sign-in page),
`GRYVIA_SESSION_TTL_SECONDS`, `GRYVIA_SESSION_SECRET`, `OIDC_ENABLED`, `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`,
`OIDC_AUDIENCE`, `GRYVIA_OIDC_ADMIN_GROUPS` (`apiGateway.oidc.adminGroups`), `GRYVIA_OIDC_LEGACY_NAMESPACES`
(`apiGateway.oidc.legacyNamespaces`), `GRYVIA_JOB_NAMESPACE`, `PROMETHEUS_URL` (`apiGateway.prometheusUrl`),
`GRYVIA_COLLECTOR_URLS`, `GRYVIA_FLIGHT_TOKEN` (`apiGateway.flightTokenSecret`), `GRYVIA_FLIGHT_COLLECTOR_NAMESPACE`,
`GRYVIA_NETRA_URL` / `GRYVIA_NETRA_TOKEN` / `GRYVIA_NETRA_INSECURE` (`apiGateway.netra.*`),
`GRYVIA_INTELLIGENCE_ACTIONS` (`apiGateway.intelligenceActions`), `CORS_ALLOWED_ORIGINS`,
`HTTP_TIMEOUT_SECONDS`.

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
