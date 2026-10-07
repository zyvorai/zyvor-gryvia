# Gryvia API Gateway

REST API service (FastAPI) that provides aggregated metrics and cluster data for the Gryvia web dashboard. The full route list with the access rule of each route is in the
[API reference](../../website/docs/developer-guide/api-reference.md); the route code is `main.py` and `routers/*.py`.

## Features

- **Cluster Statistics**: Real-time GPU availability, utilization, and job counts
- **GPU Metrics**: Historical GPU utilization, temperature, and memory usage
- **Cost Analysis**: Budget tracking, spending by team and GPU type
- **Job Metrics**: Job statistics by status and framework
- **Quota Usage**: Team quota utilization and budget tracking
- **Node Health**: GPU node health monitoring

## API Endpoints

### Cluster Stats
```
GET /api/cluster/stats
```
Returns overall cluster statistics including GPU counts, utilization, and job counts.

### Jobs CRUD
```
GET  /api/jobs              # List jobs (?limit=500&offset=0, limit up to 1000)
POST /api/jobs              # Create a new job
POST /api/jobs/preflight    # Dry-run Kubernetes admission for a job (nothing is created)
GET  /api/jobs/{name}       # Get job details
DELETE /api/jobs/{name}     # Delete a job
GET  /api/jobs/{name}/pods   # Pods of the job
GET  /api/jobs/{name}/logs   # Pod log (?pod=&tail=200, max 2000)
GET  /api/jobs/{name}/events # Events for the job and its pods
POST /api/jobs/{name}/resize # {"nodes": N}: resize a running elastic job
```

- `create_job` (POST) validates `apiVersion` and `kind` against known Gryvia types and enforces the namespace server-side.
- List endpoints take `limit` and `offset` (defaults 500 for jobs, quotas and nodes; 100 for node health and quota usage; maximum 1000).

### Quotas
```
GET /api/quotas             # List quotas (?limit=500&offset=0)
GET /api/quotas/{name}      # Get quota details
```

### Nodes
```
GET /api/nodes              # List nodes (admin only; ?limit=500&offset=0)
GET /api/nodes/{name}       # Get node details
```

### GPU Metrics
```
GET /api/metrics/gpu?time_range=1h
```
Returns GPU metrics over time (admin only). Time ranges: `1h`, `6h`, `24h`, `7d`, `30d`.

### Cost Metrics
```
GET /api/metrics/costs
```
Returns cost analysis including breakdown by team and by GPU type. The response includes a `hasHistoricalData` field indicating whether Prometheus historical data is available. Monthly cost trend data has been removed (historical trends require a Prometheus instance).

### Job Metrics
```
GET /api/metrics/jobs?time_range=24h
```
Returns job statistics including counts by status and framework.

### Quota Usage
```
GET /api/quota/usage
```
Returns quota usage across all teams.

### Node Health
```
GET /api/nodes/health
```
Returns GPU node health status (admin only).

### Roles and tenant isolation

- The API key and dashboard sessions are the provider **admin**. An OIDC user is a **tenant** user unless its
  `groups` claim contains a value from `GRYVIA_OIDC_ADMIN_GROUPS` (comma separated, default none).
- A tenant user's `org` claim (or, without `org`, its `groups` values) must match a `GryviaTenant` by name or by
  namespace `tenant-<name>`. Its namespaces are those of the matched tenants only; no match is a 403. The tenant list is cached for 30 s.
  `GRYVIA_OIDC_LEGACY_NAMESPACES=1` keeps the old "claim = namespace" behaviour, and only while no `GryviaTenant` exists.
- `GET /api/auth/me` returns `role` (`admin`|`tenant`), `tenant`, `tenants` and `tenantNamespaces`.
- Admin only (403 for tenants): nodes, node health, `/api/metrics/gpu`, `/api/network/*`, `/api/security/*`,
  `/api/ai/*`, `/api/gpu/memory`, and all writes to `/api/skus` and `/api/tenants`.
- Admin and tenants both allowed, with a namespace check: `GET /api/flight/jobs/{job}` (see below). The admin (API key or session) reads jobs, workspaces, workflows and the like from one namespace, `GRYVIA_JOB_NAMESPACE` (the chart sets it to the release namespace; `default` when unset).
- Tenant-scoped (a tenant sees only its own namespaces): jobs, workspaces, models, inference, workflows, tuners,
  `/api/quotas`, `/api/quota/usage` (quotas whose `spec.namespaces` intersect), `/api/metrics/costs`,
  `/api/metrics/jobs`, `/api/cluster/stats` (jobs only, no node capacity), `/api/usage`.

### Audit trail

Every `POST`/`PUT`/`PATCH`/`DELETE` is recorded after it completes, including failed logins and 401/403 refusals: time, request id, auth method, role, tenant, OIDC subject, method, route, path, status, outcome (`success`, `denied`, `error`), peer address and duration. Never the `Authorization` header, query string or body. The client address is the socket peer, not `X-Forwarded-For`.

- **Log:** one JSON object per line on the `gryvia.audit` logger. This is the durable record; ship it with your log collector.
- **File:** `GRYVIA_AUDIT_FILE=/path/audit.jsonl` also appends the lines (mode 0600). An unwritable file is logged, never fails a request.
- **Database:** `GRYVIA_AUDIT_DB=/data/audit.db` also stores entries in SQLite (WAL, mode 0600) on a persistent volume, so they survive restarts and can be searched. `GRYVIA_AUDIT_RETENTION_DAYS` (default 0, keep all) prunes older rows. One file, one writer: use a single gateway replica or one file per replica. An unusable database is logged and the buffer is used instead.
- **API:** `GET /api/audit?limit=&outcome=&method=&since=&until=&actor=&path=&status=` (admin) reads the database when `GRYVIA_AUDIT_DB` is set, otherwise a bounded per-replica buffer (`GRYVIA_AUDIT_BUFFER`, default 1000, lost on restart). `GET /api/audit/export.csv` takes the same filters (plus a larger `limit`) and returns CSV with spreadsheet formulas neutralised.
- **Metric:** `gryvia_gateway_audit_events_total{method,outcome}`.

The API key and dashboard sessions are one shared admin identity, so those entries say "admin via api_key/session", not who.

### GPU catalog, tenants, usage
```
GET    /api/skus[/{name}]        # any user; tenants see enabled SKUs, limited to spec.allowedSkus of their tenant
POST   /api/skus                 # admin: {name, gpuType, gpusPerUnit, hourlyRate, currency, spotDiscount, description, enabled}
PUT    /api/skus/{name}          # admin: same fields without name
DELETE /api/skus/{name}          # admin
GET    /api/audit                # admin: recent state-changing requests (?limit&outcome&method)
GET    /api/federations[/{name}] # admin: GryviaFederation cluster health (read-only, no credentials)
GET    /api/experiments[/{name}] # GryviaLiveExperiment leaderboards (tenant-filtered, read-only)
GET    /api/tenants[/{name}]     # admin: all; tenant: its own
POST   /api/tenants              # admin: {name, displayName, allowedSkus, maxGPUs, isolated}
DELETE /api/tenants/{name}       # admin
GET    /api/usage?tenant=&from=&to=&groupBy=tenant|sku|day   # {items:[{key,gpuHours,cost,currency,jobs}], totals}
GET    /api/usage/export?format=csv|json&tenant=&from=&to=   # per-record rows, attachment download
GET    /api/invoices?month=YYYY-MM&tenant=                   # {month, items:[invoice]}, one per tenant with usage
GET    /api/invoices/{tenant}/{YYYY-MM}?format=json|csv      # one invoice; 404 if no usage (csv: INV-<tenant>-<YYYYMM>.csv)
POST   /api/invoices/{tenant}/{YYYY-MM}/finalize             # admin, GRYVIA_BILLING_LEDGER=1: freeze into a GryviaInvoice
POST   /api/invoices/{tenant}/{YYYY-MM}/void                 # admin, {"reason"}: void a Finalized invoice (kept)
GET    /api/ledger/{tenant}/verify                           # recompute the tenant's ledger hash chain
POST   /api/invoices/{tenant}/{YYYY-MM}/stripe               # admin, sk_test_ key only: send to Stripe test mode
POST   /api/billing/stripe/webhook                           # Stripe-Signature checked; invoice.paid -> Paid
```
Usage comes from `GryviaUsageRecord` objects (metered estimates from job wall-clock time; no billing). Tenant users are
always limited to their own tenant, whatever `tenant` says. `/api/metrics/costs` uses usage records when any exist and
otherwise computes from jobs, priced from `GryviaGpuSku` (a built-in table only when no SKU exists).

### Flight Recorder and Netra

```
GET /api/flight/jobs/{job}?namespace=   # merged node-local Flight Recorder timeline; needs GRYVIA_FLIGHT_TOKEN
GET /api/network/flows                  # Netra flows when GRYVIA_NETRA_URL is set, else service-graph edges (admin)
```
The Flight Recorder route signs a request to each Running collector pod in `GRYVIA_FLIGHT_COLLECTOR_NAMESPACE` (default
`gryvia-network`) and merges the answers with a `coverage` field; it answers 503 without the token or without a
reachable collector. See [docs/flight-recorder.md](../../docs/flight-recorder.md). Netra: `GRYVIA_NETRA_URL`,
`GRYVIA_NETRA_TOKEN`, `GRYVIA_NETRA_INSECURE=1` (skip TLS verification).

Invoices are computed on demand from usage records (nothing stored, no payments): records are bucketed by the UTC month
of `spec.start`, one line per SKU (GPU type when the SKU is empty), `status` is always `estimate`, `open: true` marks
that some included record is still running, and `currency` is `MIXED` (with `mixedCurrency: true`) when currencies
differ. Missing `month` means the current UTC month; a bad one is 400. Tenant users only ever see their own tenant; asking
for another tenant's invoice is a 404.

With the billing ledger (`GRYVIA_BILLING_LEDGER=1`, chart `billing.ledger.enabled`) a Finalized or Paid `GryviaInvoice`
replaces the estimate (`status` `finalized`/`paid`, number `INV-<tenant>-<NNNNNN>`) and the list response carries
`billingLedger: true`. Stripe needs `GRYVIA_STRIPE_SECRET_KEY` starting with `sk_test_` and
`GRYVIA_STRIPE_WEBHOOK_SECRET`; see [docs/billing-ledger.md](../../docs/billing-ledger.md).

## Development

### Prerequisites

- Python 3.11+
- Access to Kubernetes cluster with Gryvia installed
- Prometheus with DCGM exporter (optional for detailed metrics)

### Local Development

```bash
# Install dependencies
pip install -r requirements.txt

# Run the service
python main.py

# Or use uvicorn directly
uvicorn main:app --reload --port 8080
```

The API will be available at `http://localhost:8080`.

API documentation (Swagger UI): `http://localhost:8080/docs`

### Docker

```bash
# Build the image
docker build -t gryvia-api-gateway:1.0.0 .

# Run the container
docker run -p 8080:8080 \
  -v ~/.kube/config:/home/apigateway/.kube/config:ro \
  gryvia-api-gateway:1.0.0
```

## Deployment

### Kubernetes

```bash
kubectl apply -f ../../manifests/deploy/api-gateway-deployment.yaml
```

The service will be exposed internally at `https://gryvia-api-gateway.gryvia-system:8080` (self-signed certificate; clients must skip verification or trust it).

### Configuration

The service is configured via environment variables:

- `GRYVIA_API_KEY`: the admin API key (no key means API-key sign-in is disabled)
- `GRYVIA_API_KEY_SECRET`: the Secret holding that key (default `gryvia-api-key`); `/api/auth/config` returns it so the sign-in page can show the `kubectl` command that reads the key
- `PROMETHEUS_URL`: Prometheus endpoint (default: `http://prometheus-operated.gryvia-system:9090`)
- `GRYVIA_JOB_NAMESPACE`: namespace the admin's job routes read and write (default: `default`; the chart sets the release namespace)
- `OIDC_ENABLED`, `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_AUDIENCE`, `GRYVIA_OIDC_ADMIN_GROUPS`, `GRYVIA_OIDC_LEGACY_NAMESPACES`: see [Authentication and TLS](../../website/docs/guides/AUTH_AND_TLS.md)
- `GRYVIA_SESSION_TTL_SECONDS` (default 28800), `GRYVIA_SESSION_SECRET` (the token signing key is derived from it, or from `GRYVIA_API_KEY` when unset, with PBKDF2-HMAC-SHA256; changing either ends all sessions)
- `GRYVIA_COLLECTOR_URLS`, `GRYVIA_FLIGHT_TOKEN`, `GRYVIA_FLIGHT_COLLECTOR_NAMESPACE`, `GRYVIA_NETRA_URL`, `GRYVIA_NETRA_TOKEN`, `GRYVIA_NETRA_INSECURE`
- `GRYVIA_TLS_CERT`, `GRYVIA_TLS_KEY`: serve HTTPS (the chart sets them)
- `CORS_ALLOWED_ORIGINS`: comma-separated origins (default: the local dev origins)
- `HTTP_TIMEOUT_SECONDS`, `LOG_LEVEL`

## Integration with Web UI

The Web UI proxies requests to this API gateway. Configure the Web UI's Vite proxy:

```typescript
// vite.config.ts
server: {
  proxy: {
    '/api': {
      target: 'http://localhost:8080',
      changeOrigin: true,
    },
  },
}
```

In production, configure nginx to proxy `/api` requests to the API gateway service.

## Implementation Notes

- All Kubernetes API calls are async, executed via `run_in_executor` to avoid blocking the event loop.
- `datetime.now(timezone.utc)` is used instead of the deprecated `datetime.utcnow()`.
- The Prometheus client is optional and initialized inside a `try/except` block; the service operates without Prometheus for basic CRUD functionality.
- Jobs, quotas, nodes, node health and quota usage lists paginate with `limit` and `offset`.

## Metrics Collection

The API gateway collects data from multiple sources:

1. **Kubernetes API**: CRD objects (jobs, quotas, nodes)
2. **Prometheus**: GPU metrics from DCGM exporter (optional)
3. **Node Status**: GPU health from GryviaGpuNode CRDs

## Security

- Runs as non-root user; Kubernetes RBAC limits what it can read and write.
- Authentication is built in: API key, signed session tokens and (optionally) OIDC JWTs; every `/api/` route except
  `/api/auth/config` and `/api/auth/login` requires one. Roles (admin or tenant) are described above.
- Sign-in and API routes are rate limited per client address.
- CORS allows only the origins in `CORS_ALLOWED_ORIGINS` (local dev origins by default).
- The gateway has no `/metrics` endpoint. Health check: `curl http://localhost:8080/health` (HTTPS when a certificate
  is configured).

## Performance

No throughput or latency figures have been measured for the gateway; treat any such number as unverified. Kubernetes
calls run in an executor so they do not block the event loop.

## License

Apache 2.0
