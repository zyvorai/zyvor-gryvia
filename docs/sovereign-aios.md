# Sovereign AI OS

An on-premises AI platform built from three Zyvor products, installed as one Helm release
([`helm/sovereign-aios`](../helm/sovereign-aios)) and run without any public cloud:

- **Gryvia** (this repository): GPU scheduling, model serving, the [LLM gateway](llm-gateway.md), [RAG](rag.md),
  [agents](agents.md), [datasets](datasets.md), the model registry, tenants, metering.
- **Zyntra**: a business ontology over Gryvia's resources, KPI gaps, ranked actions, approvals by named people and a
  signed audit trail.
- **Netra**: eBPF network observability and enforcement under the GPU fabric.

It covers the same layers as a vendor "sovereign AI OS" (Palantir's AIOS reference architecture with NVIDIA), from
components you can read and run disconnected. It is early: see
[What is verified and what is not](#what-is-verified-and-what-is-not).

## Layers

| Layer | Vendor stack | Here |
| --- | --- | --- |
| Hardware | NVIDIA HGX B300, Spectrum-X | Same hardware; [`ansible/`](../ansible), [`terraform/bare-metal`](../terraform/bare-metal), NVIDIA GPU and Network Operator sub-charts |
| Hardened Kubernetes | Rubix | RKE2 with the CIS profile and FIPS checks ([`ansible/roles/rke2_hardened`](../ansible/roles/rke2_hardened)), or any Kubernetes >= 1.30; Netra for network visibility and policy; opt-in Kyverno, cosign, Falco, SPIRE and OpenBao ([Hardening](#hardening)) |
| Delivery and air gap | Apollo | Helm; a Zarf package, a Harbor mirror with signatures and an Argo CD app of apps ([Air gap](#air-gap)) |
| Models, agents, retrieval | AIP | Gryvia LLM gateway, `GryviaInferenceService` (vLLM, llama.cpp), `GryviaAgent`, `GryviaVectorIndex` (Qdrant) |
| Model hub | AIP Hub | `GryviaModelRegistry`, the [model factory](model-factory.md) |
| Ontology and decisions | Foundry ontology, actions | Zyntra: objects with provenance, typed actions, approvals, signed decisions |
| Data integration | Foundry pipelines | `GryviaDataset` (http, s3, nfs), Zyntra connectors (REST, SQL, Kubernetes, files) |

## How the parts are wired

```text
 Zyntra ──REST connectors (Gryvia API key)──▶ Gryvia API gateway ──▶ Gryvia CRDs (jobs, nodes, models, ...)
   │  ──KPIs: /api/cluster/stats, /api/metrics/costs ─┘
   │  ──KPIs: /api/v1/ebpf/health, /metrics ─────────▶ Netra
   │  ──Ask, drafts (platform LLM key) ──────────────▶ Gryvia LLM gateway ──▶ vLLM / llama.cpp
   └──approved actions: GryviaPriority, GryviaGPUSharingPolicy, job suspend ──▶ Kubernetes API
 Netra ──AI briefs (platform LLM key) ───────────────▶ Gryvia LLM gateway
 Gryvia console ──/api/sovereign (agents' viewer token) ─▶ Zyntra, Netra
 GryviaAgent ──zyntra tool (service token: search, propose) ─▶ Zyntra
```

The chart does this wiring:

- **One credentials Secret** (`sovereign-aios-credentials`): the Gryvia API key (Gryvia's gateway uses it through
  `gryvia.auth.existingSecret`, Zyntra sends it as `ZYNTRA_GRAVIA_TOKEN`), the platform LLM key, and Netra's API and
  agent keys. Every key is generated on first install and read back on upgrades.
- **An LLM gateway key without the API**: the chart writes the SHA-256 of the platform key into the gateway's key
  namespace, the same format the ai-operator uses for agent keys. Zyntra's and Netra's model calls are metered under
  the release namespace and capped by its `GryviaQuota` like any tenant's.
- **One certificate**: Gryvia's self-signed certificate gets Netra's names (`gryvia.tls.extraSANs`) and Netra serves
  it (`netra.tls.existingSecret`), so Zyntra verifies both with `SSL_CERT_FILE` pointing at that one certificate. No
  client skips verification.
- **The Zyntra pack** ([`files/zyntra-pack`](../helm/sovereign-aios/files/zyntra-pack)) is mounted from a ConfigMap.
  It turns Gryvia tenants, GPU nodes, AI jobs, models, inference services and datasets into Zyntra objects, refreshed
  every one to five minutes, and Gryvia and Netra numbers into KPIs.

Zyntra's in-cluster Kubernetes client only reads built-in kinds, so it reads Gryvia's custom resources through the
Gryvia gateway, which also applies Gryvia's tenant scoping.

Approved changes are written by Zyntra itself (`zyntra.execute: apply`, the default here): its image has no
`kubectl`, so it sends the server-side apply, merge patch or delete to the Kubernetes API with its service account.
`zyntra.kubernetes.actions` grants exactly that: create, patch and delete `gryviapriorities` and
`gryviagpusharingpolicies`, and patch `gryviaaijobs` (suspend). Deletes only touch objects labelled
`app.kubernetes.io/managed-by=zyntra`. Set `zyntra.execute: dry-run` to have approvals validated by the API server
without changing anything.

## Install

Gryvia, Zyntra and Netra must be checked out side by side (or set `ZYNTRA_DIR` and `NETRA_DIR`): Netra does not
publish its chart, so the dependencies are copied from the checkouts.

```sh
make sovereign-aios-test       # copies the charts, then the render tests
make sovereign-aios-install    # helm upgrade --install sovereign-aios helm/sovereign-aios -n gryvia-system
```

The release must go into Gryvia's namespace (`gryvia.namespace.name`, `gryvia-system` by default): Zyntra and Netra
mount Gryvia's certificate and the credentials Secret, which cannot cross namespaces. The chart refuses another
namespace.

Then publish a model on the gateway (a `GryviaInferenceService` annotated `gryvia.io/llm-model`, see
[LLM gateway](llm-gateway.md)) and set `zyntra.ai.model` and `netra.ai.model` to its name. Until then Zyntra and Netra
give their deterministic answers.

## Agents on the ontology

A `GryviaAgent` with a [`zyntra` tool](agents.md#zyntra-tools) searches the ontology this release builds and drafts
proposals that people approve in Zyntra. The agent authenticates with a Zyntra service token, which Zyntra limits to
the viewer and proposer roles, so it cannot approve.

The chart makes that token. It is generated once (`zst_` and 64 hex digits) and kept in the credentials Secret as
`AGENT_ZYNTRA_TOKEN`. Its SHA-256 is added to `service_tokens` in Zyntra's policy: the chart merges `zyntra.policy`
with the token entry and writes the result to the Secret `zyntra.policyExistingSecret`, which Zyntra reads. Each
namespace in `agentToken.namespaces` gets the token in Secret `zyntra-agent-token` (key `token`):

```yaml
agentToken:
  namespaces: [tenant-alpha]
  roles: [viewer, proposer]       # [viewer] for read-only agents
  tenant: ""                      # bind the token to one Zyntra tenant
```

The tool's `url` is `http://zyntra.gryvia-system.svc:8080` (the chart names Zyntra's objects `zyntra`). The agent's
NetworkPolicy opens egress to it; if `zyntra.networkPolicy.enabled` is set, add the agent's namespace to
`zyntra.networkPolicy.ingressFrom`. Zyntra does not restart when only the token's roles or tenant change; restart its
Deployment. Service tokens and `policyExistingSecret` are in Zyntra's 0.4.0 chart and image from `main`.

## Console

The Gryvia console has a **Sovereign AI OS** page (Platform, `/sovereign`, admins only). The gateway probes Zyntra and
Netra from inside the cluster (`GET /api/sovereign`), so the browser needs neither their addresses nor their tokens:
Zyntra's version, whether it applies approved changes, its sources, open gaps and proposals waiting for approval (read
with the agents' viewer and proposer token), and Netra's health. Each card links to the product's console when
`gryvia.apiGateway.sovereign.zyntraConsoleURL` and `netraConsoleURL` are set. On the Agents page, an answer that names
a proposal links to its decision page in Zyntra.

A Gryvia installed on its own can point the page at a Zyntra and a Netra installed separately. Netra is checked with
the gateway's Netra TLS setting, so a self-signed Netra needs `apiGateway.netra.insecureTLS` or a CA. Without a viewer
token, Zyntra shows as healthy but the gaps and proposals counts stay empty. With `scripts/deploy-remote.sh`:

```bash
GRYVIA_ZYNTRA_URL=http://zyntra.zyntra.svc:8080 GRYVIA_ZYNTRA_CONSOLE_URL=http://<host>:<zyntra-nodeport> \
GRYVIA_NETRA_URL=https://netra.netra-system.svc:30870 GRYVIA_NETRA_INSECURE=1 \
GRYVIA_NETRA_CONSOLE_URL=https://<host>:30870 \
  ./scripts/deploy-remote.sh <host> <user>   # GRYVIA_ZYNTRA_TOKEN_SECRET: Secret (key `token`) with a viewer token
```

## Hardening

All off by default; each needs its operator (and CRDs) installed first. [`deploy/argocd/sovereign-aios`](../deploy/argocd/sovereign-aios)
installs them in order, at the versions pinned in [`deploy/airgap/charts.yaml`](../deploy/airgap/charts.yaml).

| Piece | Values | What it does |
| --- | --- | --- |
| RKE2 | [`ansible/playbooks/sovereign-aios-rke2.yaml`](../ansible/playbooks/sovereign-aios-rke2.yaml) | RKE2 with `profile: cis` (its sysctls, the etcd user, `protect-kernel-defaults`), encrypted Secrets, Pod Security `restricted` with the node agents' namespaces exempt, API audit logs (full bodies for `gryvia.io` writes, metadata for Secrets), etcd snapshots, a Harbor registry mirror, online or air-gapped install. FIPS: RKE2's binaries use a FIPS 140 validated module; the role requires the host kernel in FIPS mode and can switch RHEL-family hosts (`rke2_fips_enable_os`) |
| Kyverno and cosign | `hardening.kyverno` | `sovereign-aios-allowed-registries` (images only from `allowedRegistries`) and `sovereign-aios-verify-images`: Gryvia, Zyntra and Netra images must carry the cosign signature of their release workflow (keyless, GitHub OIDC), or of your key after mirroring (`mode: key`). Audit by default, `failureAction: Enforce` to reject |
| Falco | `hardening.falco`, [`deploy/hardening/falco-values.yaml`](../deploy/hardening/falco-values.yaml) | Rules for a shell in a platform container, an agent runtime starting a program or connecting to a public address or reading a service account token, and Zyntra starting a program ([rules](../helm/sovereign-aios/files/falco/sovereign-aios-rules.yaml)). falcoctl is off: no rule downloads |
| SPIRE | `hardening.spire` | `ClusterSPIFFEID`s: platform pods get `spiffe://<trust domain>/ns/<ns>/sa/<sa>`, agents `spiffe://<trust domain>/agent/<ns>/<agent>`. The services do not use the SVIDs for mTLS yet |
| OpenBao | `hardening.openbao` | The keys come from one OpenBao KV v2 entry (`gryviaApiKey`, `llmKey`, `netraApiKey`, `netraAgentKey`, `agentZyntraToken`) instead of being generated: External Secrets writes every Secret the chart would, the hashes in Zyntra's policy and the LLM gateway included. Gryvia's TLS certificates and Zyntra's admin Secret are still generated by their charts |

## Air gap

[`deploy/airgap/images.txt`](../deploy/airgap/images.txt) lists every image the release and the add-ons run
(`make sovereign-aios-images` regenerates it). Images that workloads pull at run time (training jobs, model servers you
deploy) are yours to add. Two ways in:

- **Zarf**: [`deploy/airgap/zarf.yaml`](../deploy/airgap/zarf.yaml), the platform plus optional add-on components.
  `zarf package create deploy/airgap` on the connected side, `zarf package deploy` on the other after `zarf init`.
- **Harbor and Argo CD**: `scripts/airgap-mirror.sh pull DIR` saves the images with their cosign signatures and the
  charts; `scripts/airgap-mirror.sh push DIR <harbor> --sign-key cosign.key` loads them as
  `<harbor>/<path without the registry host>`, re-signs the Zyvor images with your key, and pushes the charts to
  `oci://<harbor>/charts`. The platform uses [`values-airgap.yaml`](../deploy/airgap/values-airgap.yaml) (every image
  from Harbor, Kyverno enforcing the mirror and your key); the add-ons pull through the nodes' registry mirror
  (`rke2_registry_mirror`). [`deploy/argocd/sovereign-aios/root.yaml`](../deploy/argocd/sovereign-aios/root.yaml) syncs
  it all from Harbor and your Git mirror.

`make sovereign-aios-airgap-test` checks offline that these agree: with `values-airgap.yaml` every image is the
mirrored path of a listed image and no URL leaves the cluster, the Zarf package carries exactly `images.txt`, and
Zarf and Argo CD pin the same chart versions.

## Hardware tiers

| Tier | Nodes | GPUs | Network | Use |
| --- | --- | --- | --- | --- |
| Dev | kind or one k3s host | none (llama.cpp on CPU) | any | the chart, the pack, the wiring |
| One scalable unit | 4 GPU nodes, 3 control-plane nodes | 32 (8 per HGX node) | 400G east-west, RDMA | one site, training and serving |
| Full | 32 scalable units, 128 GPU nodes | 1,024 | rail-optimized 400G fabric, Spectrum-X or InfiniBand | multi-team training and national-scale serving |

The scalable unit follows the vendor reference architecture (4 HGX nodes per unit, up to 128 nodes). Gryvia's
[fabric scheduling](fabric-scheduling.md) places jobs by GPU type, NVLink/NVSwitch and RDMA labels; Netra watches the
host network under it.

## What is verified and what is not

| Verified | How |
| --- | --- |
| The whole loop on a cluster: the release installed on kind (Gryvia and Zyntra built from source); Zyntra reads Gryvia's objects through the real gateway over TLS with the shared key; a `GryviaAgent` with the chart's token Secret, through its NetworkPolicy, searches the ontology and proposes `raise-inference-priority` (recorded as `service:gryvia-agents`, pending); its token gets 403 approving; a person's approval makes Zyntra create the `GryviaPriority` through the Kubernetes API, labelled managed by Zyntra; the console's `/api/sovereign` reads Zyntra with the viewer token; a real model (Qwen2.5-0.5B on llama.cpp, CPU) behind the LLM gateway calls the Zyntra search tool and names a GPU node from the ontology | [`.github/workflows/e2e-sovereign-aios.yml`](../.github/workflows/e2e-sovereign-aios.yml) on every pull request touching the platform |
| The chart renders with all three products: the shared credentials, the gateway key hash (SHA-256 of a `gk-` key), Zyntra's pack, URLs, certificate and keys, the agents' service token (its hash in Zyntra's policy merged with `zyntra.policy`, the token in each agent namespace), Netra's certificate and keys, the console's wiring to Zyntra and Netra, Zyntra applying approved changes with its RBAC, Netra and Zyntra switched off, and the namespace guard | [`scripts/tests/sovereign-aios-chart.test.sh`](../scripts/tests/sovereign-aios-chart.test.sh) (`make sovereign-aios-test`), in the Sovereign AI OS workflow |
| An agent's zyntra tool against a real Zyntra wired by the chart, without a cluster: Zyntra built from source serves the pack against a stand-in Gryvia gateway with the policy and token the chart rendered; the reference runtime, driven by a scripted model, finds and reads Gryvia objects, proposes `raise-inference-priority`, is refused the approver-only `enable-mig-sharing`, gets 403 approving its own proposal, and a person's approval succeeds; an unknown token gets 401 | [`scripts/tests/sovereign-aios-agent-zyntra.sh`](../scripts/tests/sovereign-aios-agent-zyntra.sh) (`make sovereign-aios-agent-test`), in the Sovereign AI OS workflow |
| Hardening renders correctly: with OpenBao, External Secrets templates produce exactly the Secrets the chart would generate (same data and labels, hashes included); the Kyverno policies, Falco ConfigMap and SPIRE IDs; key mode needs a key | the same test |
| Zyntra's in-cluster actions: server-side apply, merge patch, labelled delete and server dry run against a fake API server, refusal of other kinds, verbs and unlabelled objects | `go test ./internal/executor` in Zyntra |
| The Kyverno registry policy passes every workload the chart renders and refuses a Docker Hub image | Kyverno CLI 1.19.1 in the Sovereign AI OS workflow |
| The Falco rules load with Falco's default rules | `falco -V` in the Falco 0.45.0 image, same workflow |
| The air-gap artifacts agree: `images.txt` holds every image (add-ons included, regenerated in CI), `values-airgap.yaml` sends all of them to Harbor and no URL leaves the cluster, the Zarf package carries exactly that list, Zarf and Argo CD pin the same versions; Zarf lints the package | [`scripts/tests/sovereign-aios-airgap.test.sh`](../scripts/tests/sovereign-aios-airgap.test.sh), `scripts/sovereign-aios-images.sh --check`, `zarf dev lint`, same workflow |
| The RKE2 role passes `ansible-lint` (production profile) and its templates render the CIS, Pod Security, audit and mirror settings for a first server, a joining server and an agent | [`ansible/tests/rke2_hardened_templates.yaml`](../ansible/tests/rke2_hardened_templates.yaml), same workflow |
| The console's overview: the gateway endpoint (healthy, no token, unreachable, unhealthy, Netra probed with its own TLS setting) and the page | `services/api-gateway/tests/test_sovereign.py`, `web-ui/src/pages/Sovereign.test.tsx` |
| The overview against a standalone Zyntra and Netra on k3s: both healthy, Zyntra's version, mode and 6 of 6 sources, console links | `scripts/deploy-remote.sh` to a lab host with the variables above (manual, not in CI) |
| The pack is valid for Zyntra 0.4: 8 KPIs, 6 object types, 3 link types, 2 typed actions, 3 views | `zyntra pack validate helm/sovereign-aios/files/zyntra-pack` |

| Not verified | Why |
| --- | --- |
| Netra in the end-to-end run | Its eBPF agent is not run on the CI runner's kind; Netra is installed by the chart but switched off there |
| The hardening add-ons running: Kyverno admitting or rejecting pods, signature checks against the registry, Falco raising alerts, SPIRE issuing SVIDs, External Secrets syncing from OpenBao | Rendered and checked by each tool's validator, not installed on a cluster |
| The RKE2 role on real hosts, FIPS mode on real kernels | Linted and rendered only; CI has no VMs to provision |
| Building and deploying the Zarf package, the mirror script against a real Harbor, an Argo CD sync | Linted and cross-checked offline; not run end to end |
| Per-object actions (suspend this job) | Zyntra typed actions take fixed parameters; passing the chosen object's name and namespace into a Gryvia template needs a Zyntra change |
| Objects with the same name in two tenants | AI jobs and datasets are keyed by name, so two tenants' jobs of the same name are one object |
| Anything on GPUs, RDMA or at the scale of the tiers above | No GPU hardware in CI, as for the rest of Gryvia |

## Licenses

Gryvia, Zyntra and Netra are all under the Zyvor Production Licence v2.0, which is not an open-source license, so
the release is source-available, not open source. Check that the combination fits your use before you deploy it.
