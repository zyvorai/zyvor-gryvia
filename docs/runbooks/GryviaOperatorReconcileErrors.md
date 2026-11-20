# GryviaOperatorReconcileErrors

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

A controller's `controller_runtime_reconcile_errors_total` rate exceeds 0.05/s for 15 minutes.

## What it means

A Gryvia operator controller keeps failing to reconcile its objects, so status and side effects (quota accounting, usage records, jobs) may be stale.

## How to check

```bash
sum by (controller) (rate(controller_runtime_reconcile_errors_total[10m]))   # which controller
kubectl -n gryvia-system get pods; kubectl -n gryvia-system logs <operator-pod> | grep -i error | tail -30
kubectl get events -A --sort-by=.lastTimestamp | tail -20
kubectl auth can-i --list --as=system:serviceaccount:gryvia-system:<serviceaccount> | head
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Missing RBAC after an upgrade.
- CRD schema mismatch between chart and operator version.
- An object with invalid spec that fails every retry (look at the object named in the log).
- API server throttling.

## Mitigation

- Fix the specific error in the operator log (RBAC, CRD, bad object).
- Ensure CRDs match the chart version: `kubectl apply --server-side -f helm/gryvia/crds/` (Helm does not upgrade CRDs).
- `kubectl rollout restart deploy/<operator>` after fixing.

## Limits (honest)

Errors are counted per controller, not per object; retries of one bad object can dominate.
