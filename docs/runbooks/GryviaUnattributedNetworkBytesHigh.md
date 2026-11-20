# GryviaUnattributedNetworkBytesHigh

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

Unattributed network bytes exceed 100 KB/s and more than half of all accounted bytes for 30 minutes.

## What it means

The network cost meter saw traffic it could not assign to a tenant (unknown or non-tenant peer, hostNetwork, loopback, same-node). Network cost and usage numbers under-count; the meter deliberately fails closed rather than guess.

## How to check

```bash
sum(rate(gryvia_netcost_unattributed_bytes_total[15m])); sum by (zone_type) (rate(gryvia_network_cost_bytes_total[15m]))
kubectl -n gryvia-network logs <collector-pod> | grep -i netcost
docs/network-cost-attribution.md
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Workloads on hostNetwork or traffic through NAT/SNAT (a documented limit).
- Pods not in tenant namespaces generating most of the traffic.
- Collector's pod/node directory not synced (RBAC or API errors).

## Mitigation

- Check that the collector can list pods and nodes (its ClusterRole); look for RBAC errors in its log.
- If the traffic is legitimately non-tenant (system pods, hostNetwork), raise the alert threshold or silence it.

## Limits (honest)

SNAT'd and hostNetwork traffic can never be attributed with the current design. The metric counts bytes, not money.
