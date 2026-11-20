# GPU as a Service

Run Gryvia as a multi-tenant GPU cloud: you (the **provider**) onboard **tenants**, publish a **catalog** of GPU
SKUs with prices, tenants self-serve GPU workloads in their own isolated namespace, and Gryvia **meters** each
tenant's GPU hours and cost.

:::note What this is, and is not
Costs are **metered estimates** from job run time (GPUs x hours x the SKU's hourly rate). Gryvia generates
**estimate invoices** (a monthly statement per tenant, as JSON or CSV) but does not take payments or issue tax invoices.
It can reserve nodes for a tenant (opt-in, see below). Feed the invoice or usage export to your billing and payment system.
:::

For how this fits next to what a GPU cloud provider supplies (hardware, facilities, provisioning), see
[GPU cloud platform](https://github.com/zyvorai/gryvia/blob/main/docs/gpu-cloud-platform.md).

```
provider (API key / admin group)                tenant user (OIDC token)
  create tenants, publish SKUs                    sees only its own jobs, usage and quotas
        │                                                    │
        ▼                                                    ▼
 GryviaTenant ─▶ namespace tenant-<name> + ResourceQuota + NetworkPolicy
 GryviaGpuSku  (price sheet)  ─┐
 GryviaAIJob   (run time)     ─┴─▶ usage controller ─▶ GryviaUsageRecord ─▶ /api/usage, CSV/JSON, dashboard, CLI
```

## 1. Configure sign-in

Turn on OIDC ([Authentication](./AUTH_AND_TLS.md#single-sign-on)) and make your identity provider put the tenant in
the token: the `org` claim, or the tenant among the `groups`. Optionally list your operators' group in
`apiGateway.oidc.adminGroups`. The API key remains the provider admin.

## 2. Publish the catalog

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaGpuSku
metadata:
  name: h100-80g
spec:
  gpuType: H100
  hourlyRate: 8        # per GPU-hour
  currency: USD
  spotDiscount: 60     # percent, informational
```

Or `gryvia catalog`, the dashboard **Catalog** page (admin can add and edit), or `POST /api/skus`. With no SKUs at
all, Gryvia prices jobs from a built-in default table so a fresh install still shows costs.

## 3. Onboard a tenant

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaTenant
metadata:
  name: acme
spec:
  displayName: Acme Robotics
  allowedSkus: [h100-80g]      # empty = every enabled SKU
  quotas:
    concurrentGPUs: 8
  networkPolicy:
    isolated: true
```

The tenant controller creates the namespace `tenant-acme` with a ResourceQuota, LimitRange and (when isolated) a
NetworkPolicy. Jobs whose GPU type is not allowed are rejected by the quota operator
(`GpuTypeNotAllowed` / `NoEnabledSku`) wherever a `GryviaQuota` covers the namespace, and the admission webhook denies
such jobs up front with the reason in the error.

## 4. What a tenant sees

A tenant user sees only its own tenant: jobs, workspaces, inference, workflows, tuners, quotas, usage and the
catalog (enabled SKUs, limited to `allowedSkus`). Cluster-wide pages (nodes, network, security, GPU metrics) are
admin-only and answer 403.

## 5. Metering and export

One `GryviaUsageRecord` per job (`usage-<job uid>` in the job's namespace) records GPUs, start, end, GPU hours, rate
and cost. A job that Kueue evicts and admits again gets one record per run (`usage-<job uid>-<n>` after the first), so
queued time is not billed. A running job's record is refreshed every minute; when the job ends it is finalised and never changed again: a validating webhook
(`quotaOperator.usageRecordWebhook.enabled`) rejects edits to the `spec` of a finished record. Deleting and recreating
a record is not blocked.

```bash
gryvia usage --group-by tenant
gryvia usage --tenant acme --from 2026-09-01 --to 2026-09-30 --group-by day -o csv
curl -H "Authorization: Bearer $TOKEN" "https://<gateway>/api/usage/export?format=csv&tenant=acme"
```

The dashboard **Usage** page shows the same numbers with export buttons.

## 6. Monthly invoices (estimates)

An invoice is computed on demand from the month's usage records: one line per SKU with GPU hours, rate and amount, a
subtotal, and an `estimate` status (with `open: true` while jobs are still running). Nothing is stored; the same
records always give the same invoice. Tenants only ever see their own.

```bash
gryvia invoice --month 2026-09
gryvia invoice --tenant acme --month 2026-09 -o csv
curl -H "Authorization: Bearer $TOKEN" "https://<gateway>/api/invoices/acme/2026-09?format=csv"
```

The dashboard **Invoices** page shows the same statements with CSV and JSON download.

## Payments

Gryvia does not process payments. Take the CSV/JSON invoice into your billing system, or poll
`/api/invoices?month=YYYY-MM` from it, and let that system charge the tenant. As a provider-neutral seam an admin can
`POST /api/invoices/{tenant}/{month}/send` to push the invoice JSON, HMAC-signed, to the URL in
`GRYVIA_INVOICE_WEBHOOK_URL` (off unless set). A Stripe-style integration needs a provider decision and is not built.

## Budgets, reservations and tenant RBAC (opt-in)

- **Budgets** are computed from the metered usage records (`GryviaBudget`, `GryviaQuota.spec.budget`). With
  `aiOperator.admissionGate=true` a job whose forecast cost would push a hard budget over its limit is rejected before
  anything is created.
- **Reservations** (`quotaOperator.reservations=true`) taint and label nodes for the owning tenant; the owner's jobs opt in with
  the annotation `gryvia.io/reservation`.
- **Tenant RBAC** (`quotaOperator.tenantRbac=true`) gives each tenant's members Kubernetes access to their own namespace only.

Details, semantics and limits: [docs/gpuaas-completion.md](https://github.com/zyvorai/gryvia/blob/main/docs/gpuaas-completion.md).

## Limits

- Estimates only; restarted, preempted or spot jobs are metered by wall-clock time.
- Enforcement needs a `GryviaQuota` covering the tenant namespace; a namespace without one is not enforced.
- Admission checks (allowed GPU types, per-job GPU limit, the tenant's allowed SKUs) run when a job is created and
  fail open: if the quotas cannot be read, the job is admitted and the quota operator's reactive enforcement rejects
  it shortly after. Only new jobs are checked, never updates. Concurrent-job limits are enforced reactively; budgets are
  also checked before creation when `aiOperator.admissionGate` is on (off by default), from estimates.
- Verified with unit tests and fake clusters; it has not yet run against a real identity provider or GPUs.
