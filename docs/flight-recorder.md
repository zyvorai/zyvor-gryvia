# Gryvia Flight Recorder (node-local preview)

The collector now keeps a bounded timeline of eBPF observations attributed to a
`GryviaAIJob`. The collector uses the host process cgroup's pod UID and the
Kubernetes pod list for its node. It never assigns an event to a job when the
pod cannot be resolved. The pod cache expires after one minute without a
successful API refresh.

Install with the Flight overlays (or set the same flags by hand):

```bash
helm upgrade --install gryvia oci://ghcr.io/zyvorai/charts/gryvia -n gryvia-system -f helm/gryvia/values-flight.yaml …
helm upgrade --install gryvia-net … -f helm/network-intelligence/values-flight.yaml …
```

Or enable `ebpf.enabled=true` on the network-intelligence chart. Create a
random token of at least 32 characters and store it as a Secret named
`gryvia-flight-token` with key `token` in both `gryvia-network` (collector)
and `gryvia-system` (gateway). Set `ebpf.flightTokenSecret=gryvia-flight-token`
on the network-intelligence chart and
`apiGateway.flightTokenSecret=gryvia-flight-token` on the Gryvia chart. The
token values must be identical; the Secret objects live in separate namespaces.
The collector diagnosis API returns 503 when its token is not configured.

The collector needs its existing pod-list RBAC, host PID access and `/host/proc`
mount. Inspect each node's attachment status first:

```sh
kubectl -n gryvia-network get pods -l app.kubernetes.io/component=collector -o wide
kubectl -n gryvia-network port-forward pod/<collector-pod> 9090:9090
curl -s localhost:9090/api/v1/ebpf/status
```

The response identifies its node and says `node-local; observed events only`.
The authenticated gateway merges node reports at
`GET /api/flight/jobs/train?namespace=ml`. Its `coverage` field distinguishes
reachable collectors, reporting collectors, and the total discovered; a partial
view is never marked complete. Tenant callers can query only their assigned
namespaces. The most recent 500 events across nodes are returned.
When at least two pod ranks each have three NCCL calls of the same operation,
the gateway reports their observed median API durations and flags large
outliers. This comparison cannot identify the underlying GPU or network cause.
`coverage.complete` means every discovered collector answered; nodes without a
running collector pod are not counted, and `truncated` is true when a node
returned its 200-event limit or more than 500 events were merged.

## Command line and dashboard

Both read the gateway's cluster view (`GET /api/flight/jobs/{job}?namespace=`),
so neither needs the collector token; that token stays between the gateway and
the collectors. Both show a partial view as partial: `coverage.complete` is
false when a discovered collector did not answer, and `truncated` is true when
events were dropped.

`gryvia-flight` (source in `collector/cmd/gryvia-flight`, built with
`cd collector && go build ./cmd/gryvia-flight`; the collector image does not
contain it) runs anywhere that can reach the gateway, for example through
`kubectl -n gryvia-system port-forward svc/<gateway> 8080:8080`. HTTPS is
required except for `localhost`. The credential is the gateway API key, a
session or an OIDC token; it is never a flag value. Provide it with
`-token-file FILE`, `-token-file -` (stdin) or `GRYVIA_API_TOKEN`:

```text
gryvia-flight -gateway http://localhost:8080 -namespace ml -job train -token-file ~/.gryvia-token
gryvia-flight -gateway https://gryvia.example -namespace ml -job train -o text -require-complete
```

`-o json` (default) prints the gateway response; `-o text` prints a summary.
Exit codes: 0 fetched, 1 request failed (401 rejected token, 403 namespace not
allowed for a tenant token, 503 gateway token unset or no collector reachable),
2 usage error, 3 partial or truncated with `-require-complete`. TLS is verified
and redirects are not followed, so the token is never sent elsewhere.

The dashboard shows the same report in the Flight Recorder card on a job's
detail page, for the job's own namespace. It loads only when you press Load
observations (each load contacts every collector) and is not polled. A tenant
user without access to the namespace sees a 403 message.

Opt-in request tracing (W3C `traceparent` ids attached to events, `GET /api/flight/trace/{traceId}`) and engine latency
(`gryvia-flight -inference`) are described in [inference-latency.md](inference-latency.md).

## Collector authentication

The gateway signs each collector request with
`hex(HMAC-SHA256(token, METHOD "\n" REQUEST-URI "\n" UNIX-SECONDS))`, sent in
`X-Gryvia-Flight-Signature` and `X-Gryvia-Flight-Time`. The token itself is
never transmitted. The collector compares in constant time, accepts timestamps
within 30 seconds of its own clock (keep node clocks in sync), reads the token
file on every request (a rotated Secret applies without a restart), requires at
least 32 characters and answers 503 when the token is missing or too short.
Rejections are bare status codes; the token and signatures are never logged.

Limits to be aware of:

- A signed request can be replayed unchanged for up to 30 seconds by anyone who
  can observe it. The signature also covers no host. This is accepted because
  the endpoint is read-only and idempotent; it is not a defence against a
  network eavesdropper. Collectors use plain HTTP inside the cluster, so use
  network isolation or transport encryption where event confidentiality
  matters.
- The gateway discovers collectors from Running pods labelled
  `app.kubernetes.io/component=collector` in `apiGateway.flightCollectorNamespace`
  (default `gryvia-network`) only, and skips pod IPs that are not IP addresses.
  Anyone who can create pods in that namespace can receive signed requests, so
  keep it operator-only. `GRYVIA_COLLECTOR_URLS`, if set, replaces discovery and
  is an administrator setting; no request parameter ever selects a URL.
- The fan-out uses 3 s per collector, a 10 s overall deadline, 16 concurrent
  requests, no redirects, no proxy environment and a 2 MiB response cap.
  Collectors that fail or time out are reported through `coverage`.
- Tenant (OIDC) callers may query only their tenant namespaces (403 otherwise);
  the provider admin may query any namespace. Job and namespace names must be
  DNS-1123 labels.
Events are retained in memory (at most 2,048 per job and 128 jobs per node),
and the endpoint returns the latest 200. It returns 404 when no events were
attributed on this node. It is not a recording of actual GPU occupancy, RDMA
wire bytes, or complete training steps. Confirm GPU activity with DCGM/CUPTI;
NCCL and RDMA probes still require hardware validation.

For TCP connections observed from open to close, `tcp_retransmit` records the
number of retransmissions at close. The kernel can attribute retransmits that
occur in softirq context to the owning process captured at connection open.
Connections already open when the program attaches are not included.

## Identity caveats

- Attribution trusts the `gryvia.io/job` pod label set by the AI operator, so
  anyone who can create pods in a namespace can label a pod into that
  namespace's job timeline (never into another namespace's).
- The PID is resolved when the event is processed, not when it was emitted. A
  process that exits and whose PID is reused within that short gap could be
  mis-resolved; an exited process is simply dropped.
- Resolution needs `hostPID` and the `/host/proc` mount. When the collector runs
  in a cluster without that mount it resolves nothing rather than falling back
  to its own PID namespace.
- `counts` and `findings` cover the events returned (the latest 200), not the
  whole retained timeline.

## Restricting access

Only `GET /api/v1/flight/diagnose` requires a token (see below). The rest of the
collector's HTTP listener (`:9090`: metrics, `/api/v1/ebpf/status`,
`/api/v1/fabric`, the GPU, AI and tuning endpoints) is still served without
request authentication. It carries job names and traffic timing, so do not
expose it outside the cluster operators.

- Prefer `kubectl port-forward` (as above); it needs RBAC on the pod.
- Do not create a Service, Ingress or LoadBalancer for the collector.
- Restrict pod ingress on port 9090 to the monitoring namespace, for example:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: collector-restrict
  namespace: gryvia-network
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/component: collector
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
      ports:
        - port: 9090
```

- The chart defaults to `collector.hostNetwork: true`, where the port is bound on the
  node's addresses and a pod NetworkPolicy does not apply. Use node firewall
  rules, or set `collector.hostNetwork=false` before relying on the policy above.

GPU/RDMA/NCCL validation on real hardware still precedes general availability. Collective identity (communicator
ordinal, sequence, rank) is now learned from public NCCL API uprobes, but only against a stand-in library so far;
see [nccl-rdma-gpu-correlation.md](nccl-rdma-gpu-correlation.md).

See [flight-diagnosis.md](flight-diagnosis.md) for the unified bottleneck diagnosis, persistent incident history and measurement completeness.

See [nccl-rdma-gpu-correlation.md](nccl-rdma-gpu-correlation.md) for collective identity (the `collectives` list), RDMA NIC counters and DCGM GPU correlation, and [network-cost-attribution.md](network-cost-attribution.md) for tenant network cost attribution.
