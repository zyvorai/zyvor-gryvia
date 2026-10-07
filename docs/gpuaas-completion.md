# GPU-as-a-service completion: budgets, reservations, tenant RBAC, invoice webhook

What was added on top of tenants, the SKU catalog and usage metering, how it behaves, and where it stops.
Everything below is covered by Go/Python/TypeScript/Rust unit tests against fake clients. Nothing has run on GPUs or
against a real identity provider; the Kubernetes-level behaviour (taints, RoleBindings, the admission gate) is
exercised only by the kind workflow `.github/workflows/e2e-gpuaas.yml` (`scripts/e2e-gpuaas.sh`), which passes in CI.

## What is opt-in

| Feature | Switch (Helm value / flag) | Default |
|---|---|---|
| Budget controller (`GryviaBudget` status, reactive reject when blocked) | always registered | on (only acts on `GryviaBudget` objects) |
| Admission gate | `aiOperator.admissionGate` / `--admission-gate` (+ `aiOperator.admissionDefaultHours` / `--admission-default-hours`, 1) | off |
| Reservations | `quotaOperator.reservations` / `--enable-reservations` | off |
| Tenant RBAC | `quotaOperator.tenantRbac` / `--tenant-rbac` (ships the ClusterRoles, adds operator RBAC) | off |
| Invoice webhook | gateway env `GRYVIA_INVOICE_WEBHOOK_URL` (+ `_SECRET`) | off (503 when unset) |

With every switch off the chart renders exactly as before (checked by rendering both and comparing).

## 1. Budgets

### Where the numbers come from

Spend is the sum of `GryviaUsageRecord.spec.cost` for a **scope** in a **period** (`operators/quota-operator/pkg/spend`).
Records are per job, priced when written from the enabled `GryviaGpuSku` (or the default table when the cluster has no
SKUs), with their own rate and currency. **Open (non-final) records count** with the cost they hold now. A record that
straddles the period start is prorated by the share of its wall-clock time inside the period.

`distributed` jobs are counted as `nodes x gpusPerNode` GPUs everywhere (`GryviaAIJobSpec.TotalGPUs()`): the usage
record producer, the tenant/chargeback/quota-policy/cost-predictor code and the admission gate. The usage record's
`gpus` and `gpuHours` are therefore right at the source, which is what the gateway usage and invoice routes read.

| Scope (`GryviaBudget.spec.scope.type`) | Records counted |
|---|---|
| `namespace` | records in that namespace |
| `tenant` | records of that tenant (label/`spec.tenant`) and namespace `tenant-<name>` |
| `team` | namespaces of every `GryviaQuota` with that team, plus records attributed to a tenant of that name |
| `user`, `project` | not supported: the budget reports `ScopeUnsupported` and enforces nothing |

Periods: `daily`, `weekly` (from Sunday), `monthly` (default), `quarterly`, `annual`, `custom` (`startDate`/`endDate`
as `YYYY-MM-DD`, end date inclusive, or RFC3339). A custom period that has ended or not yet started does not apply to
new jobs.

`GryviaQuota.status.budgetStatus` (`spentThisMonth`, `remainingBudget`, `percentUsed`, `projectedSpend`) is computed by
the same code for the calendar month over the quota's namespaces.

### Currency

Budget limits are USD (`costUSD`, `monthlyBudget`). If the records in a scope are in **mixed currencies, or all in one
non-USD currency**, the spend cannot be compared with the limit: the budget is **reported, not enforced**
(`Enforceable=False` condition on `GryviaBudget`; a warning from the gate), and `percentUsed` stays 0. No conversion is done.

### State machine (`GryviaBudget.status.state`)

Computed from the utilization percentage and the spec only (never from the previous state):

| State | When |
|---|---|
| `blocked` | the budget blocks (`enforcement.enabled: true`, `action: block`) and utilization reached the block point: 100 %, or the lowest alert threshold that carries the action `block` |
| `exceeded` | utilization >= 100 % and the budget is not (yet) blocking |
| `warning` | utilization >= the lowest alert threshold (80 % when there are no alerts) |
| `active` | otherwise |

Utilization is the larger of cost % (`limits.costUSD`) and GPU-hours % (`limits.gpuHours`). `maxConcurrentJobs` is shown
in `utilization.jobsPercent` but does not drive the state. Alerts are recorded once per period per threshold. Not
implemented and ignored: `enforcement.gracePeriod`, `rollover`, `priority` overrides.

The controller recomputes every minute (records refresh every minute), so status lags real spend by about two minutes.
When a budget is `blocked` it also rejects jobs still `Pending`/`Queued` in the scope (reactive fallback).

### Admission gate

With `--admission-gate` the AI operator asks, before creating a job's PVC, Service or workload (one call at the top of
the workload-creation path, `controllers/gryviaaijob_admission.go` and `pkg/admission`), whether the job may exist.
It reads everything as unstructured objects (the operators are separate Go modules; `pkg/admission/spend.go` is a copy of
the quota operator's `pkg/spend` and must stay in step) and reuses the webhook's quota policy code.

It rejects (phase `Rejected`, message with the reason, condition `Rejected` with reason `QuotaExceeded` or
`BudgetExceeded`, event `AdmissionRejected`; nothing is created) when:

- the GPU type is not in the quota's `allowedGPUTypes`, or the tenant has no enabled SKU among `allowedSkus`;
- the job's total GPUs exceed `maxGPUsPerJob`;
- GPUs held by `Scheduling`/`Running` jobs in the quota's namespaces plus this job exceed `maxGPUs` (this rejects; it
  does not queue: queueing is the Kueue workstream's job);
- a **hard** budget would be exceeded by `spend + forecast`. Hard means `GryviaQuota.spec.budget.hardLimit: true`
  (monthly, limit `monthlyBudget`) or a `GryviaBudget` with `enforcement.enabled` and `action: block` (limit
  `costUSD x blockPoint`, and `gpuHours` likewise).

**Forecast** = total GPUs (`nodes x gpusPerNode`) x hours x the matching SKU's hourly rate, where hours is
`spec.timeout` or `--admission-default-hours` (1). The job is assumed to run for its whole timeout.

**Soft** budgets (no `hardLimit`, or enforcement not `block`) only add a `BudgetWarning` condition and a Warning event
(once per distinct message). A budget that cannot be checked (mixed/non-USD currency, GPU type no SKU prices) is also
a warning, and the job is allowed.

Rules of the road:

- It runs only for jobs in phase `Pending` (never started). A job that is `Scheduling` or later is never re-judged, and
  `Rejected` is sticky, so raising a budget later does not un-reject a job (resubmit it).
- **Fails open.** Any read error (missing CRD, RBAC, timeout) lets the job through with a Warning event and an
  `AdmissionUnchecked` condition, exactly like the webhook. The operators ClusterRole already grants every read the
  gate needs, so the chart adds no RBAC for it.
- CPU-only jobs (`gpus: 0`) are never gated.

## 2. Reservations

`GryviaReservation` (cluster-scoped) now reserves nodes for real. Enable the controller with
`quotaOperator.reservations=true` (**turning it on makes any existing GryviaReservation start tainting nodes**).

While the schedule window is open the controller claims **Ready, schedulable** nodes labelled `gryvia.io/gpu=<gpuType>`
until `gpuCount` GPUs are covered, counting `nvidia.com/gpu` allocatable per node (fallback: the
`gryvia.io/gpu-count` label; a node with neither counts 0, no "8 GPUs" assumption). Or it takes exactly
`resources.nodes` if listed. Selection is deterministic (nodes it already holds first, then by name) and all-or-nothing
for a first allocation: an unsatisfiable reservation stays `pending` with a `status.message` and holds nothing. A held node
that goes NotReady is released and replaced when another qualifies.

A claim is an optimistic-locked Node patch, so two reservations racing for a node cannot both win (the loser re-reads,
sees the other's label and picks again). On each claimed node:

- taint `gryvia.io/reserved=<owner>:NoSchedule` (value = owner name sanitized to a label value);
- labels `gryvia.io/reserved-for=<reservation name>` (the claim), `gryvia.io/reserved-by=<owner>`, and
  `gryvia.io/exclusive=true` when `guarantees.exclusive` (informational: every reservation is enforced by the taint).

Taint and labels are removed on expiry, when the reservation is deleted (finalizer; this is how a reservation is
**cancelled**, `gryvia reservation cancel` and `DELETE /api/reservations/{name}` delete the object) and for nodes whose
reservation no longer exists (swept on every reconcile). Other taints on the node are never touched.

Reservation names must be at most 63 characters (they become a label value).

### Schedules

- `immediate`: active now, until `endTime` if set.
- `scheduled`: active from `startTime` until `endTime`.
- `recurring`: `recurrence.cron` (five fields, UTC; `*`, numbers, ranges, `*/n`, lists; no names or macros; day-of-month
  and day-of-week are OR-ed when both are restricted) opens a window of `recurrence.duration` (`8h`, `90m`, `2d`) at each
  fire time. Between windows the state is `pending` and the nodes are free. `startTime`/`endTime` bound the recurrence.
  Implemented with a small parser (`operators/quota-operator/pkg/cron`), no dependency.
- `autoExtend`: past `endTime` the nodes are kept while a job of the reservation is `Running` or `Scheduling`.

Status: `state` `pending|active|expired`, `allocatedNodes`, `allocatedGPUs`, `message`, and a job count plus reserved
time in `utilizationMetrics` (no utilization percentage). `cancelled` exists in the CRD but is never written: cancel means delete.

### Using a reservation

A job opts in with the annotation (or label) `gryvia.io/reservation: <name>`. The AI operator (`buildPodTemplate` calls
`applyReservation`) then adds a toleration `gryvia.io/reserved=<owner>:NoSchedule` and a nodeSelector
`gryvia.io/reserved-for=<name>`. The reservation's `spec.owner` must match the job: owner type `tenant` (name = tenant
or `tenant-<name>`), `team` (the job's or its namespace's `gryvia.io/team` label, or the tenant name), or `namespace`.
Otherwise, or if the reservation does not exist or has expired, the annotation is **ignored** and the job gets a
`Reservation=False` condition (reason `OwnerMismatch`, `NotFound`, `Ended`) plus a Warning event.

A job **without** the annotation gets nothing and simply cannot land on the tainted nodes: that is the enforcement, and it
also applies to the owner's own jobs that forgot the annotation. The API server does not know about owners: a user who
can create pods directly (tenants cannot: RBAC below) could add the toleration themselves; the taint is scheduling
hygiene, not an authorization boundary against cluster users with pod-create rights on those nodes.

Node RBAC needed (`nodes` get/list/watch/update/patch) was already in the operators ClusterRole; nothing was added.

## 3. Per-tenant RBAC

Set `quotaOperator.tenantRbac=true`. The chart then ships three ClusterRoles (`helm/gryvia/templates/tenant-rbac.yaml`) and
the tenant controller binds them with **RoleBindings** in `tenant-<name>`, one per role that has subjects, named
`gryvia-tenant-<role>` and labelled `gryvia.io/managed-by=tenant-controller` (only those are ever pruned or edited; an
unmanaged object of the same name is refused, not overwritten).

| Rule | viewer | member | admin |
|---|---|---|---|
| get/list/watch `gryviaaijobs`, `gryviaworkspaces`, `gryviainferenceservices`, `gryviaworkflows`, `gryviaautotuners`, `gryviamodelwatches`, `gryviamodelregistries`, `gryviausagerecords`, `gryvianetworkusagerecords`, `gryviafabricsignals` | yes | yes | yes |
| create/update/patch/delete `gryviaaijobs`, `gryviaworkspaces`, `gryviainferenceservices`, `gryviaworkflows`, `gryviaautotuners`, `gryviamodelwatches`, `gryviamodelregistries` | no | yes | yes |
| get/list/watch `pods`, `pods/log`, `events`, `services`, `configmaps` | yes | yes | yes |
| create `pods/portforward` | no | yes | yes |
| create/update/patch/delete `configmaps`, `services` | no | no | yes |
| `pods/exec`, `secrets`, quotas, limit ranges, network policies, role bindings | no | no | no |
| write `gryviausagerecords` | no | no | no (billing source of truth) |

Decisions: **no `pods/exec`** (a shell in a GPU pod with the job's data and credentials; use logs). **`pods/portforward` for
member/admin** because workspaces have no ingress; trade-off: it reaches any port of any pod in the tenant namespace.
`GryviaQuota`, `GryviaBudget`, SKUs and reservations are cluster-scoped and cannot be granted through a RoleBinding: tenants
see them only through the gateway.

Subjects: `spec.members[]` become `User` subjects named `username` (the name your API server's OIDC `--oidc-username-claim`
produces, including any `--oidc-username-prefix`); `spec.oidcGroups[]` become `Group` subjects with role
`spec.oidcGroupRole` (default `member`). An empty or unknown member role becomes **viewer** and is reported in the
`RBACReady` condition (reason `UnknownRole`); a user listed twice keeps the highest role. Removing a member, or emptying a
role, deletes the stale binding on the next reconcile (within two minutes, or immediately on a spec change).

Operator RBAC added when enabled: `rolebindings` get/list/watch/create/update/patch/delete, and `bind` on `clusterroles`
restricted by `resourceNames` to the three ClusterRoles. Kubernetes escalation prevention means the operator can grant
exactly those roles and nothing else.

**This is Kubernetes-side access only.** The API gateway enforces its own model (API key, sessions, OIDC claims resolved to
tenants in `routers/tenancy.py`) and does not read these RoleBindings; the two are configured separately. For RoleBindings
to matter, tenants must reach the Kubernetes API with an identity the API server maps to those names (OIDC on the API
server). No identity provider was involved in testing: the e2e workflow uses `kubectl --as`.

## 4. Invoice webhook (the payments seam)

**Gryvia does not process live payments or issue tax invoices.** (The opt-in [billing ledger](billing-ledger.md) adds finalized, immutable invoices and a Stripe integration that accepts test-mode keys only.) The seam is a provider-neutral webhook:
`POST /api/invoices/{tenant}/{month}/send` (admin only) builds the estimate invoice JSON (the same builder as
`GET /api/invoices/...`) and POSTs it to `GRYVIA_INVOICE_WEBHOOK_URL` (503 while unset). If `GRYVIA_INVOICE_WEBHOOK_SECRET` is
set the body is signed:

```
X-Gryvia-Timestamp: <unix seconds>
X-Gryvia-Signature: sha256=<hex HMAC-SHA256(secret, "<timestamp>.<body>")>
```

Bounded 10 s timeout, redirects are not followed (a 3xx is reported as not delivered), the URL comes only from the
environment, must be https unless the host is localhost, and link-local/metadata, multicast, unspecified and other non-routable
addresses are refused (every resolved address is checked; a DNS-rebinding race between the check and the connection is not
closed). The response is `{delivered, status, error}`. Sending is idempotent on your side only if your receiver de-duplicates
on tenant and month. Connecting this to Stripe or any invoicing product needs a provider decision and a real integration
(customer mapping, tax, retries, reconciliation); none of that exists.

## Gateway, dashboard, CLI

- Gateway: `GET/POST /api/reservations`, `DELETE /api/reservations/{name}`, `GET /api/budgets`. Tenants see only reservations whose
  owner is their tenant and budgets scoped to their tenant or its namespace; admins see everything; a tenant can create only
  reservations owned by itself.
- Dashboard: **Reservations** and **Budgets** pages.
- CLI: `gryvia reservation list|create|cancel`, `gryvia budget` (read through kube, like `gryvia quota`).

## Limits

- Estimates only: spend is wall-clock time x rate from records refreshed once a minute; the forecast assumes the whole timeout.
  Concurrent submissions can each pass before any shows up in the records.
- The admission gate fails open, is opt-in, and covers only jobs not yet started. Soft budgets never block.
- Mixed or non-USD currencies are reported, not enforced. No currency conversion.
- `user` and `project` budget scopes, `gracePeriod`, `rollover` and `priority` overrides are not implemented.
- Reservations taint nodes; they are not an authorization boundary against users with pod-create rights, and nodes are held
  whole (no GPU-level sharing). A tainted node is unusable by the owner's jobs that lack the annotation.
- RBAC is Kubernetes-side; the gateway enforces its own model. Nothing verified against a real IdP.
- No payments. See section 4.
