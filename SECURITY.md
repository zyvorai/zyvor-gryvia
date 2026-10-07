# Security policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Use GitHub's private
vulnerability reporting on this repository (Security > Report a vulnerability),
and include steps to reproduce and the affected version.

We aim to acknowledge reports within a few business days. Gryvia is early-stage
software; fixes ship in the next release and are noted in the changelog.

## Supported versions

Only the latest release on `main` receives security fixes.

## Threat model and known limits

Gryvia is alpha software. Read this before exposing it beyond a lab. It is written from the code; nothing here has been
penetration tested, and the OIDC and tenant paths have only been tested against a fake identity provider.

### Sign-in, roles and tenants

- **Two roles.** The API key and the browser sessions issued by `POST /api/auth/login` are the provider **admin**: full
  control of every route, including tenants, SKUs, network and security data. There is only one such identity (`admin`),
  so no per-user attribution or audit trail exists for key-based sessions. OIDC users are **tenant** users, limited to
  the namespaces of the `GryviaTenant` objects they match; `GRYVIA_OIDC_ADMIN_GROUPS` (chart
  `apiGateway.oidc.adminGroups`, empty by default) promotes a group to admin. Cluster-wide routes (nodes, network,
  security, GPU metrics) are admin-only. The route-by-route rules are in the
  [API reference](https://zyvorai.github.io/gryvia/docs/developer-guide/api-reference).
- **Tenant isolation is enforced by the gateway, not by Kubernetes RBAC.** A `GryviaTenant` creates a `tenant-<name>`
  namespace with a ResourceQuota, a LimitRange and (when `networkPolicy.isolated`) a NetworkPolicy. It creates no Role
  or RoleBinding. Anyone who has Kubernetes credentials that can read or write `gryvia.io` objects bypasses the gateway's
  filtering entirely, so do not hand tenants kubeconfigs for the provider cluster unless you add your own RBAC.
- **OIDC tenant matching trusts your identity provider's claims.** The token's `org` claim (or, when there is no `org`,
  each entry of `groups`) is matched against tenant names and `tenant-<name>` namespaces. Whoever can set those claims at
  the provider can place a user in any tenant, and a group named like a tenant grants that tenant. Unmatched tokens get
  403. `apiGateway.oidc.legacyNamespaces=true` restores the old behaviour (the claim is a namespace) and takes effect
  only while no `GryviaTenant` exists; leave it off. Keep the signing-key, issuer and audience checks of your provider
  correct; the gateway validates signature, issuer, audience and expiry.
- **The admin sees one namespace for job routes.** Job, workspace, workflow and similar routes read the gateway's
  `GRYVIA_JOB_NAMESPACE` (the release namespace in the chart) for the admin; that is a convenience, not a security
  boundary, since the admin can read everything else through other routes or `kubectl`.
- **Metering is an estimate.** `GryviaUsageRecord` objects are computed by the quota operator from job wall-clock time
  and are custom resources. Once a record is sealed (`spec.final: true`, set when its job finishes) a validating webhook
  (`quotaOperator.usageRecordWebhook.enabled`, on by default) rejects any change to its `spec`, including clearing
  `final`; creates, deletes, labels and annotations are not checked. That protects a finished record from edits, not
  from being deleted and recreated by someone allowed to do both, and a running record can still be changed until it is
  sealed. Invoices are computed on demand from the records and there is no payment processing.
- **Admission checks fail open.** The `GryviaAIJob` validating webhook (`webhook.failurePolicy=Ignore` by default)
  admits jobs when the operator is down, and its quota and catalog policy admits a job when the quotas cannot be read.
  The quota operator's reactive enforcement rejects a violating job shortly after. Set `webhook.failurePolicy=Fail` if
  you prefer to block instead. The usage-record webhook is the exception: its failure policy is `Fail`, so while the
  quota operator is down, updates to usage records are rejected.

### The shared key and sessions

- **Well-known lab default.** Installs default to the key `Admin@321` so a quick start works. The dashboard shows a
  warning while it is in use. **Change it** for anything reachable from an untrusted network: `--set auth.apiKey=<secret>`,
  an existing Secret, or `auth.apiKey=""` to generate a random one. `scripts/deploy-remote.sh` keeps the key that is
  already installed on a redeploy and uses the default only on a first install.
- **Login hardening.** `POST /api/auth/login` compares credentials in constant time, slows failed attempts and is rate
  limited per client address (10 per minute). It returns a signed session token that expires after 8 hours
  (`GRYVIA_SESSION_TTL_SECONDS`), so the browser never stores the API key; rotating the key (or `GRYVIA_SESSION_SECRET`)
  ends all sessions. The signing key is derived from the API key (or the session secret) with PBKDF2-HMAC-SHA256.
  There is no server-side session revocation for a single session.
- **Rate limiting** trusts `X-Forwarded-For` only when the connecting peer is a private or loopback address.

### Transport

- **TLS.** The dashboard and gateway serve HTTPS. The default certificate is self-signed; use `tls.mode=certManager` or
  `existingSecret` for a trusted one. Traffic between the dashboard and the gateway inside the cluster uses the same
  certificate without verification, so enable the chart's `networkPolicy.enabled` on shared clusters.
- **Netra.** When `apiGateway.netra.url` is set, the gateway calls that URL with the optional bearer token; TLS
  verification is on unless `apiGateway.netra.insecureTLS` (`GRYVIA_NETRA_INSECURE=1`) is set.

### Cluster privileges

- **Operators and gateway.** They run with broad Kubernetes RBAC on the `gryvia.io` resources, nodes and pods (see
  `helm/gryvia/templates/rbac.yaml`). Install Gryvia only on clusters where that is acceptable.
- **Containers.** The operators, gateway and dashboard run as non-root with a read-only root filesystem, no privilege
  escalation and all capabilities dropped; the TLS Secret is mounted with `fsGroup`, so no init container needs root.
- **NVIDIA GPU Operator (`nvidia.enabled=true`).** The release namespace (`gryvia-system`) is labelled
  `pod-security.kubernetes.io/enforce: privileged` because NVIDIA's driver and toolkit pods are privileged and mount the
  host. Those pods, and any other pod you run in that namespace, are then not restricted by Pod Security admission.
  Install the sub-chart into a dedicated namespace if that matters to you. This path has not been run on real GPU
  hardware.

### The eBPF collector (experimental, off by default)

- **Privileged DaemonSet.** The collector (`helm/network-intelligence`, `ebpf.enabled=true`) runs as root, privileged,
  with `hostPID` and, by default, `hostNetwork`. It loads kernel programs and reads process and connection data for the
  whole node. Its image is in the signed release workflow, but no tagged release has published it yet; until one does, you
  build and vouch for it yourself.
- **hostNetwork.** With the default `collector.hostNetwork=true` its port 9090 listens on the node's addresses, and a pod
  NetworkPolicy does not apply. Use node firewall rules, or set `collector.hostNetwork=false` before relying on a policy.
- **Most endpoints are unauthenticated.** `/metrics`, `/healthz`, `/api/v1/ebpf/status`, `/api/v1/graph`,
  `/api/v1/anomalies`, `/api/v1/fabric`, `/api/v1/security/alerts`, the GPU, AI and tuning endpoints answer any caller
  that can reach the port. They expose job names, service names and traffic timing. Do not create a Service or Ingress
  for the collector and restrict who can reach port 9090.
- **Flight Recorder.** Only `GET /api/v1/flight/diagnose` authenticates: an HMAC-SHA256 over method, request URI and
  timestamp with a shared token (at least 32 characters), constant-time compared, valid for 30 seconds. The token is not
  sent, but a signed request can be **replayed unchanged for up to 30 seconds** by anyone who can observe it, the
  signature does not cover the host, and collectors use plain HTTP inside the cluster. The gateway sends signed requests
  to every Running pod labelled `app.kubernetes.io/component=collector` in `apiGateway.flightCollectorNamespace`, so
  anyone who can create pods in that namespace can receive them: keep it operator-only. Tenant callers of
  `GET /api/flight/jobs/{job}` are limited to their tenant namespaces; job attribution trusts the `gryvia.io/job` pod
  label, so anyone who can create pods in a namespace can label a pod into that namespace's timeline. Details and
  limits: [docs/flight-recorder.md](docs/flight-recorder.md).
- **Programs that change traffic.** The fabric-signal programs are observe-only. `packet_filter` (an XDP rule map) and
  `sockops_optimize` (sk_msg redirect) can affect traffic when they are attached (`-iface`, `-cgroup-path`). A
  mutating program that would pace or throttle traffic for quota enforcement (sometimes called quota-pace) is **not in
  this repository**; no shipped program enforces quotas in the data path.
- **Verification.** The programs were verifier-loaded on Linux 7.0 x86_64 only; arm64 is compile-only, and GPU, NCCL,
  RDMA and GPUDirect Storage behaviour has not been checked on hardware.

### Supply chain

- **Images and charts.** Release images are signed with cosign (keyless) and carry SBOM and provenance attestations;
  the OCI Helm charts are signed with cosign. The eBPF collector image is part of the release workflow (each
  architecture built natively), but no tagged release has published it yet.

See [Authentication and TLS](https://zyvorai.github.io/gryvia/docs/guides/AUTH_AND_TLS) for configuration.
