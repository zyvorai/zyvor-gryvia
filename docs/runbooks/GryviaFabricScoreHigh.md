# GryviaFabricScoreHigh

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`max by (namespace, job) (gryvia_fabric_score_delta) > 0.6` for 10 minutes.

## What it means

The collector's topology scorer computed a fabric health penalty above 0.6 (0 healthy, 1 worst) for a job on some node. The score combines the signals below (NCCL stragglers, RDMA retries, CNP, PFC, NIC errors); it is a heuristic used for opt-in fabric-aware scheduling, not a measured SLA.

## How to check

```bash
gryvia status                                   # platform overview
kubectl get gryviafabricsignals -A              # only populated when the collector runs with -publish-fabric-status
kubectl -n <ns> get gryviafabricsignal <name> -o yaml   # status shows the per-signal values
# Which signals drive it: open the 'Gryvia / Fabric status' dashboard, or query each gauge:
#   gryvia_fabric_straggler_rank, gryvia_fabric_rdma_retry_rate, gryvia_fabric_cnp_rate, gryvia_fabric_pfc_rate, gryvia_fabric_nic_error_rate
# Raw collector view (see the runbooks README, 'Calling a collector'):  GET /api/v1/fabric
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- One rank on a slow or degraded link (see GryviaStragglerPersists, GryviaNicErrors).
- RoCE congestion or PFC pausing (GryviaCnpRateHigh, GryviaPfcRateHigh).
- Transport retries from a flapping port or cable (GryviaRdmaRetryHigh).

## Mitigation

- Fix the dominant signal first; the score falls once its window (about 30 s) is clean.
- Cordon the affected node or move the job if one node is the source: `kubectl cordon <node>`.
- If fabric-aware scheduling is on (`aiOperator.fabricAwareScheduling`), new jobs already avoid the node; running jobs are not moved.

## Limits (honest)

The score weights are heuristics and unvalidated on real fleets. Values are per node window: for a job on several nodes each collector reports its own view (last writer wins in the CR status). Nothing in this repo was run against real RDMA hardware.
