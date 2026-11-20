# Securing the collector listener

The eBPF collector (`collector/`, one pod per node) serves HTTP on `-metrics-addr` (default `:9090`):
`/metrics`, `/api/v1/*` (graph, anomalies, fabric, gpu, security alerts, training, tuning, eBPF status)
and the Flight Recorder report. This page describes what protects that listener, how to turn each
protection on, and what remains exposed.

## Threat model

The listener is reachable by anything that can route to the pod. With the chart default
`collector.hostNetwork=true` that is **the node IP on port 9090**, so any host or pod that can reach the
node (and, on flat networks, anything on the node subnet) can read:

- per-flow service graph, anomalies, DNS/security alerts, NCCL and GPU statistics, fabric health and
  eBPF program status (reconnaissance value: workload names, namespaces, IPs, which security probes run);
- `/metrics` (labels carry namespaces and pod names).

Nothing on the listener writes to the cluster or changes kernel state, so the risk is disclosure and
noise (load), not control. Before this change only `/api/v1/flight/diagnose` was authenticated.

Attackers considered: a compromised pod on the same network, a curious tenant, a network-level
eavesdropper or man in the middle between gateway/Prometheus and the collector.
Not considered: an attacker who already has root on the node or can create privileged pods (see
"Residual risks").

## What exists

| Layer | Flag | Effect |
|---|---|---|
| TLS | `-tls-cert-file`, `-tls-key-file` | HTTPS, TLS >= 1.2, ECDHE + AEAD suites only. The key pair is re-read when the files change (Secret rotation, no restart); a bad rotation keeps the last good pair and logs a warning. |
| mTLS | `-tls-client-ca-file` | Clients must present a certificate that verifies against the CA bundle (also reloaded). A verified client certificate authenticates every endpoint. |
| Request signing | `-api-token-file` | Every endpoint other than `/healthz`, `/readyz` and the flight endpoint needs `X-Gryvia-Time` and `X-Gryvia-Signature`. |
| Metrics bearer | `-metrics-token-file` | `Authorization: Bearer <token>` opens `/metrics` only. |
| Enforce | `-require-auth` | Refuse to start unless one of the three credential mechanisms is configured. |
| Opt out | `-insecure-listener` | Explicitly serve everything unauthenticated (contradicts the flags above; start fails if combined). |

Defaults are unchanged: with none of these flags the listener behaves as before (plain HTTP,
unauthenticated except flight) and the collector logs `SECURITY:` warnings at start. Once any credential
mechanism is set, the middleware denies by default: every path other than `/healthz`, `/readyz` and
`/api/v1/flight/diagnose` needs a credential, including paths added later.

### Request signature

```
X-Gryvia-Time:      canonical decimal UNIX seconds (accepted skew: 30 s)
X-Gryvia-Signature: hex(HMAC-SHA256(token, "GRYVIA-API-V1\n" METHOD "\n" REQUEST-URI "\n" UNIX-SECONDS))
```

Same construction as the Flight signature but with a fixed `GRYVIA-API-V1` prefix, so a Flight signature
cannot be replayed against the generic scheme (or the reverse) even if one token is reused. The token
must be at least 32 characters and is re-read on every request, so rotating the Secret needs no restart.
The Go signer/verifier (`collector/listener_security.go`) and the gateway signer
(`services/api-gateway/routers/collector_transport.py`) are pinned together by a cross-language vector
test on each side. The Flight endpoint keeps its own token and semantics.

A signature proves the caller knows the token; it does not hide the response and a signed request can be
replayed unchanged within the 30 s window. Use it together with TLS.

## Enabling each mode (network-intelligence chart)

All under `ebpf.security` (only rendered with `ebpf.enabled=true`).

1. Signing only (cheap, no PKI): create a Secret with a random >= 32 character `token` key in the
   collector namespace and set `ebpf.security.apiTokenSecret`. Copy the same token into the gateway
   namespace and set `apiGateway.collector.tokenSecret` in the `gryvia` chart. Add `metricsTokenSecret`
   so Prometheus can scrape (bearer, `/metrics` only; the ServiceMonitor gets `bearerTokenSecret`, and the
   Secret must exist in the ServiceMonitor's namespace).
2. TLS: `tls.enabled=true` with either an existing Secret (`tls.secretName`, keys `tls.crt`, `tls.key`,
   `ca.crt`) or `tls.certManager.enabled=true` plus `issuerName`/`issuerKind` (creates a Certificate with
   `tls.certManager.dnsNames`, default `gryvia-collector`). Clients reach collectors by pod IP, so they
   verify a DNS name from that list: gateway `apiGateway.collector.serverName`, Prometheus
   `ebpf.security.serviceMonitor.serverName`. Certificates need the usual X.509 extensions (subject and
   authority key identifiers): recent Python and OpenSSL verify strictly; cert-manager sets them.
3. mTLS: also `tls.clientCA=true` (uses `ca.crt` of the Secret). Then Prometheus needs a client certificate
   (`serviceMonitor.clientCertSecret`, a Secret in the ServiceMonitor namespace) and the gateway needs one
   (`apiGateway.collector.tlsSecret` with `ca.crt`, `tls.crt`, `tls.key` and `clientCert=true`). Both must
   be signed by the CA in the collector Secret. Note the client certificate alone authenticates: with
   mTLS on, a bearer token is not needed by Prometheus.
4. Enforcement: `requireAuth=true` so a mis-typed Secret name fails at start instead of silently running open.
5. `insecureListener=true` only to record that an open listener is intended.

Gateway (`gryvia` chart `apiGateway.collector`, or env): `GRYVIA_COLLECTOR_CA_FILE` (also switches
discovered collector URLs to https), `GRYVIA_COLLECTOR_TLS=1` (https with system roots),
`GRYVIA_COLLECTOR_CERT_FILE`/`GRYVIA_COLLECTOR_KEY_FILE`, `GRYVIA_COLLECTOR_SERVER_NAME`,
`GRYVIA_COLLECTOR_TOKEN` or `GRYVIA_COLLECTOR_TOKEN_FILE`. Unset means the previous behaviour. An unreadable
CA or client certificate fails closed (collectors counted unreachable; no fallback to plain http).
The gateway's Flight token (`flightTokenSecret`) is independent and unchanged.

Direct scraping example (plain Prometheus, bearer):

```yaml
scheme: https
authorization: {credentials_file: /etc/prometheus/collector-token}
tls_config: {ca_file: /etc/prometheus/collector-ca.crt, server_name: gryvia-collector}
```

## hostNetwork: what needs it

`collector.hostNetwork` stays `true` by default. From reading the collector and eBPF sources, the
**only** thing that needs the host network namespace is attaching XDP/TCX programs to a host interface
(`ebpf.interface` / `-iface`): `packet_filter`, `dns_tracker`, the cost tracker, the trace correlator,
`roce_cnp`, `pfc_pause`, `roce_ecn` and `xdp_mux`. The loader resolves the interface with `net.InterfaceByName` in the pod's own
network namespace, where the host NIC does not exist. Nothing else depends on it: kprobes/tracepoints and
uprobes are global to the kernel, `sockops`/`sk_msg` attach to a cgroup path, and no program filters on a
network namespace. `hostPID` (and `/host/proc`, cgroup and bpffs mounts) are still required for pod
attribution and uprobes.

With `collector.hostNetwork=false` the chart refuses to render if `ebpf.interface` is set. Port 9090 is then
served on the pod IP (the gateway discovers pod IPs and is unaffected), and
`ebpf.security.networkPolicy.enabled=true` renders an ingress-only NetworkPolicy allowing port 9090 from the
gateway namespace and the Prometheus namespace (with optional pod labels). A NetworkPolicy has no effect on
hostNetwork pods, so it is not rendered in the default mode; there, rely on TLS/auth and node firewalls.

Unverified: the hostNetwork=false conclusion comes from code reading and chart rendering. It was not run on a
cluster, and CNI-specific NetworkPolicy enforcement was not tested.

## Residual risks

- The collector DaemonSet is `privileged`, runs as root, mounts `/proc`, `/sys/fs/bpf`, cgroups, debugfs and
  `/run` from the host and uses `hostPID`. A code-execution bug in the collector is node compromise. The
  listener hardening reduces exposure of that code, it does not sandbox it. Reduce the surface by leaving
  unneeded programs unattached and not enabling mutating options (`quotaPace`).
- The chart-managed token Secrets are ordinary Secrets: anyone who can read them can sign requests. Use
  RBAC, encryption at rest and rotation.
- Signed requests are replayable for 30 s; use TLS. Tokens are shared across all collectors (one leaked
  token opens every node): prefer mTLS with per-client certificates for stronger separation.
- `/healthz` and `/readyz` are open by design and reveal only liveness.
- Revocation: certificates are not checked against a CRL; rotate the CA to revoke.
- Default install is still open (warning only). Set `requireAuth=true` for production.
