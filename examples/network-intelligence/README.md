# Network Intelligence Examples

Example YAML manifests for Gryvia network intelligence CRDs powered by eBPF and Cilium.

> **Status.** The controllers read the eBPF collector (through its per-node pods), Netra (optional, `GRYVIA_NETRA_URL` on the operator) and the fabric-signal and network-usage objects; an object says `SourceAvailable=False` in its status conditions when its source is not deployed or not reachable. `GryviaFlowPolicy` (creates a CiliumNetworkPolicy) requires Cilium. Verified with unit tests and a fake collector only; not run against a real collector fleet, Hubble or Cilium. See `docs/network-intelligence-sources.md` for which controller reads what.

## Examples

- **[flow-policy.yaml](flow-policy.yaml)** - `GryviaFlowPolicy` allowing payment-to-database traffic with low-latency intent
- **[auto-policy.yaml](auto-policy.yaml)** - `GryviaAutoPolicy` in learn mode for 10 minutes, then suggesting policies
- **[trace-session.yaml](trace-session.yaml)** - `GryviaTraceSession` debugging the payment service at L7 for 2 minutes
- **[service-graph.yaml](service-graph.yaml)** - `GryviaServiceGraph` mapping service dependencies in production
- **[network-anomaly.yaml](network-anomaly.yaml)** - `GryviaNetworkAnomaly` detecting latency spikes > 100ms with auto-mitigation
- **[traffic-insight.yaml](traffic-insight.yaml)** - `GryviaTrafficInsight` analyzing payment service traffic patterns

## Usage

```bash
kubectl apply -f flow-policy.yaml
kubectl apply -f auto-policy.yaml
kubectl apply -f trace-session.yaml
kubectl apply -f service-graph.yaml
kubectl apply -f network-anomaly.yaml
kubectl apply -f traffic-insight.yaml
```
