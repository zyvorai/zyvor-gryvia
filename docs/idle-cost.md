# Idle GPU cost in chargeback

A `GryviaChargeback` normally bills each cost center only for the jobs it ran (`actual-usage`). Provisioned GPUs that
nobody used are then nobody's cost, and the chargeback total is lower than the GPU bill. The `idle-proportional` method
spreads that idle cost over the cost centers in proportion to what each one spent, the same idea as OpenCost's idle cost
(asset cost minus workload cost, optionally distributed to tenants; see [RESEARCH.md](RESEARCH.md)).

**Status:** implemented in the quota operator and covered by unit tests with a fake client. The idle amount is supplied
by you (`spec.allocationModel.idleCost`); the operator does not measure cluster capacity or read a cloud bill. Not run
against a cluster.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaChargeback
metadata:
  name: monthly-with-idle
spec:
  period:
    type: monthly
  allocationModel:
    method: idle-proportional
    idleCost: 4200.00        # cost of unused GPU capacity this period, in the pricing currency
  costCenters:
    - id: research
      teams: [ml-research]
    - id: product
      teams: [ml-product]
```

## Rules

- `idleCost` is finite, between 0 and 1e12, and only allowed with `method: idle-proportional` (otherwise the object is
  refused with a condition, never silently ignored).
- Shares are proportional to metered cost and rounded to cents with the largest-remainder rule, so the shares add up
  exactly to `idleCost`. Ties are broken by cost center ID, so the result is deterministic.
- Unassigned usage (teams in no cost center) carries no idle share.
- If no cost center has any usage there is nobody to charge: the amount is reported in
  `status.currentPeriod.idleUnallocated` and not added to the total.
- Platform overhead (`includePlatformCosts`, `platformOverhead`) applies to usage plus idle.
- Status: each cost center has `idleCost` (the part of `cost` that is idle) and `byResourceType.idle` is the total charged.

## Computing the idle amount

`idlecost.IdleCost(capacityGPUHours, allocatedGPUHours, rate)` in `operators/quota-operator/pkg/idlecost` returns
`max(0, capacity - allocated) * rate`, rounded to cents, for scripts and future controllers.

## Reclaim planner (library only)

`operators/quota-operator/pkg/fairshare` holds the decision core for fair-share reclaim: a weighted borrowing share
(`max(0, usage - nominal) / weight`) and `SelectVictims`, which chooses preemptible workloads from the most-borrowing
queues, never below a queue's nominal quota, under the `LessThanOrEqualToFinalShare` or `LessThanInitialShare`
rule. Nothing calls it yet; it exists so a reclaim controller can be built on tested logic. Inputs are size-capped
(1000 queues, 100000 workloads) and validated.
