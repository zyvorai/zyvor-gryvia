# Demo data (no GPUs required)

`demo.yaml` creates **fake** GPU nodes, a team quota and a few workloads so the dashboard is populated on a
cluster without GPUs (for example kind). The nodes are declarations only: nothing schedules real GPU work, and
the metrics you see are illustrative. Do not apply this on a production cluster.

```bash
kubectl apply -f examples/demo/demo.yaml
```

Remove it with `kubectl delete -f examples/demo/demo.yaml`.
