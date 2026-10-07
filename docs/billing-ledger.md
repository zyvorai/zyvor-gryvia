# Billing ledger, finalized invoices and Stripe test mode

Off by default. Without it, invoices stay on-demand **estimates** computed from `GryviaUsageRecord` objects and
nothing is persisted (see [GPU as a service](gpuaas-completion.md)). With it, every sealed usage record is copied into
an append-only, hash-chained ledger, and an administrator can freeze a closed month into a numbered `GryviaInvoice`.
Stripe is an optional add-on that only accepts **test-mode** keys; Gryvia cannot charge real money.

```yaml
webhook:
  enabled: true                 # required: immutability is enforced by the admission webhook
quotaOperator:
  usageRecordWebhook:
    enabled: true               # required (default)
billing:
  ledger:
    enabled: true
  stripe:
    secretName: gryvia-stripe   # optional; keys secret-key (sk_test_...) and webhook-secret (whsec_...)
```

The chart refuses to render `billing.ledger.enabled` without the webhooks.

## What it adds

| Piece | Where | Behavior |
|-------|-------|----------|
| `GryviaLedgerEntry` (cluster-scoped) | quota-operator `--billing-ledger` | One entry per sealed usage record, named `le-<record UID>`, chained per tenant |
| `GryviaInvoice` (cluster-scoped) | API gateway | `Draft` → `Finalized` → `Paid` or `Void`; lines frozen at finalization |
| Admission rules | quota-operator webhook | Ledger entries: no spec update, no delete. Invoices: only the transitions below; only a `Draft` can be deleted. Usage records: a sealed record cannot be deleted |
| `POST /api/invoices/{tenant}/{YYYY-MM}/finalize` | gateway, admin | Build lines from the ledger and freeze them |
| `POST /api/invoices/{tenant}/{YYYY-MM}/void` | gateway, admin | Body `{"reason": "..."}` (3 to 500 characters) |
| `GET /api/ledger/{tenant}/verify` | gateway, admin or own tenant | Recompute the tenant's hash chain |
| `POST /api/invoices/{tenant}/{YYYY-MM}/stripe` | gateway, admin, Stripe only | Send a finalized invoice to Stripe (test mode) |
| `POST /api/billing/stripe/webhook` | gateway, Stripe signature | `invoice.paid` moves the invoice to `Paid` |
| Invoices page | dashboard | Status badges (Estimate, Finalized, Paid, Void); Finalize and Void buttons for admins |

RBAC: the quota-operator gets `create/get/list/watch` on `gryvialedgerentries` (never update or delete). The gateway
gets `get/list` on ledger entries and `get/list/create/patch/delete` on `gryviainvoices` (the webhook allows deleting
only drafts).

## The ledger

When a usage record becomes final, the ledger controller (one reconcile at a time, leader-elected) reads the tenant's
chain head without the cache, and creates the next entry:

- `sequence` = head + 1 (1 for a new tenant), `prevHash` = the head's `hash` (empty for sequence 1);
- every amount as a decimal string with 6 places (`gpuHours`, `rate`, `cost`), times as RFC 3339 UTC seconds;
- `hash` = hex SHA-256 of these fields joined by `\n`, in this order:
  `v1, tenant, sequence, prevHash, usageRecord.namespace, usageRecord.name, usageRecord.uid, kind, job, sku, start,
  end, gpuHours, rate, cost, currency, recordedAt`.

The tenant is `spec.tenant`, or the namespace without `tenant-` (the same attribution the invoice routes use). The
record then gets the annotation `gryvia.io/ledger-entry: le-<uid>`. Because the entry name comes from the record UID,
a retry never appends a second copy. The Go controller and the gateway verifier share a golden hash test.

`GET /api/ledger/{tenant}/verify` returns `{tenant, entries, valid, problems[], headSequence, headHash, hashVersion}`.
It reports a gap or duplicate in the sequence, a `prevHash` that does not match the previous entry, and an entry whose
contents no longer match its hash. The webhook already blocks edits; verification is the second check, for example
after a restore from backup or against someone with direct etcd access.

## Finalize, void, paid

Finalizing `tenant`/`YYYY-MM` refuses with 409 when the month still has open usage records, when a sealed record is
not yet in the ledger ("retry shortly"), when the chain fails verification, when currencies are mixed, or when the
month is already Finalized or Paid. Otherwise it:

1. groups the month's ledger entries by SKU (decimal arithmetic; the subtotal is the sum of the rounded line amounts);
2. creates a `Draft` named `inv-<tenant>-<yyyymm>-<revision>` with number = the tenant's highest number + 1 and the
   ledger range (`entries`, `firstSequence`, `lastSequence`, `headHash`);
3. checks that no other invoice took the same number concurrently (if one did, the draft is deleted and the call
   answers 409 so the admin can retry);
4. patches it to `Finalized` with `finalizedAt` and `finalizedBy`.

From then on `GET /api/invoices...` returns the persisted invoice (number `INV-<tenant>-<6-digit number>`, `status`
`finalized` or `paid`, frozen lines) instead of the estimate, including in the CSV export. A finalized invoice covers
GPU (and token/Slurm) usage records only; network egress lines stay in the estimate view.

Voiding needs a reason and keeps the invoice; a voided month shows the estimate again and can be finalized again under
the next number. A `Paid` invoice cannot be voided.

The webhook enforces: `Draft` may change freely or become `Finalized` (with `finalizedAt` and `finalizedBy`).
`Finalized` may set `payment` once, become `Paid` (with `paidAt`) or `Void` (with `voidedAt`, `voidedBy`,
`voidReason`), and nothing else. `Paid` and `Void` are immutable. Metadata (labels, annotations) can always change.

## Stripe (test mode only)

Enabled only when `GRYVIA_STRIPE_SECRET_KEY` starts with `sk_test_`; any other key (including `sk_live_` and
`rk_live_`) leaves the routes answering 503. Any Stripe response with `livemode: true` is refused, and webhook events
must carry `livemode: false`.

Sending (`POST /api/invoices/{tenant}/{YYYY-MM}/stripe`) finds the customer with metadata `gryvia_tenant` or creates
one, creates a draft Stripe invoice (`collection_method=send_invoice`, 30 days, metadata `gryvia_invoice`), one invoice
item per frozen line (amount in minor units) and finalizes it. Every call carries an idempotency key derived from the
`GryviaInvoice` name, so a retried send does not duplicate anything at Stripe. The Stripe invoice id and hosted URL are
stored once in `spec.payment`; a second send answers 409.

The webhook verifies `Stripe-Signature` (`t=<unix>,v1=<hex>`, HMAC-SHA256 of `"<t>.<body>"` with
`GRYVIA_STRIPE_WEBHOOK_SECRET`, 300 s tolerance), then on `invoice.paid` matches `metadata.gryvia_invoice` and the
recorded Stripe invoice id and moves the invoice to `Paid`. Repeating the event is a no-op; other event types are
acknowledged and ignored. Point the Stripe test-mode webhook endpoint at `https://<gateway>/api/billing/stripe/webhook`.

## Tested, and what is not

- quota-operator: ledger controller (per-tenant chains, no duplicate after a lost annotation write, open records
  skipped, namespace tenant fallback, invalid tenant rejected, golden hash) and the admission rules (every invoice
  transition, ledger immutability, draft-only delete, sealed usage-record delete) with fake clients.
- Gateway: hash parity with the Go golden value, chain verification (rewrite, gap, relink), finalize/void/refinalize,
  every 409 path, tenant scoping, and Stripe with `httpx.MockTransport` (customer reuse, idempotency keys, minor units,
  live-mode refusal, signature and tolerance checks, idempotent `invoice.paid`).
- Helm render test `scripts/tests/billing-chart.test.sh` (off by default; flag, webhooks, RBAC and Stripe env only when
  enabled; refuses to render without webhooks). Dashboard helper tests.
- Not done: no run against the real Stripe test API, no tax, no credit notes, no PDF. Ledger entries are not exported to
  external WORM storage; anyone who can write etcd directly can still rewrite history, which `verify` would detect but
  not prevent.
