# Strategy: Gryvia next to an open-source Run:ai

October 2026. Internal planning note; the public comparison is
[website/docs/guides/COMPARE_RUNAI.md](../../website/docs/guides/COMPARE_RUNAI.md).

## What changed

On October 13, 2026 NVIDIA open-sourced the whole Run:ai platform under Apache 2.0: KAI Scheduler (CNCF Sandbox), a
GPU Fractioning Operator built on CUDA 13.4's driver-enforced memory limits (scheduler-agnostic), KAI Resource
Management (projects, departments, node pools) and Workload Map. Eight infrastructure partners integrate it on day one,
some of them (Saturn Cloud, Rafay) adding metering, billing and self-service catalogs on top.

"An open-source Run:ai" is therefore no longer a position. Run:ai is open, mature, NVIDIA-backed and free to
self-host. Competing on scheduler depth or GPU-fraction isolation means rebuilding, alpha and untested on GPUs, what
NVIDIA now gives away with production history.

## Position

**Gryvia turns a GPU cluster into a GPU cloud you can run as a business: tenants, price catalog, metering, budgets and
invoices, an LLM gateway, RAG and agents, in one Helm chart, on the scheduler you choose (Kueue or KAI).**

- Compete above the scheduler, where Run:ai stops and its partners sell closed add-ons: multi-tenant GPU-as-a-service
  economics and the LLM platform.
- Treat Run:ai's open components as dependencies, not rivals. A Gryvia install that uses KAI Scheduler and the GPU
  Fractioning Operator gets the best scheduling and isolation available and keeps Gryvia's layers on top.
- Keep the laptop-first experience (kind demo, no GPUs) and the honesty rule (no claim beyond what was tested) as
  differentiators in trust and adoption.

Who it is for, in order: small and mid-size GPU clouds and neoclouds that bill tenants; universities and research
computing centres that charge back GPU time and still run Slurm; enterprise platform teams that want an internal LLM
gateway with quotas next to their training cluster.

## What this change delivered

Toward parity in daily use (useful whatever the scheduler underneath):

- `gryvia submit --image ... --gpu 0.5 -- cmd`: one-line submit with fractional GPUs, MIG, multi-node and `--dry-run`.
- MPS fractions now also set the per-client device-memory limit from `gryvia.io/gpu-memory`.
- A GPU-share option on the dashboard's submit form.
- Workspaces you can open from the browser through the gateway (per-workspace sessions, WebSockets, NetworkPolicy),
  real idle detection from Jupyter and code-server, and `gryvia workspace new|list|open|pause|resume|delete`.
- The default Jupyter image was fixed (the previous one did not exist); the kind demo opens the dashboard by itself
  and, from a local build, adds a notebook.
- A "Coming from Run:ai?" command map and an honest comparison page.

All of it is unit-tested; the workspace proxy and the kind demo were also run end to end on kind (see the CHANGELOG).

## Plan

**Now: proof.** Nothing in Gryvia has run on a GPU, which outweighs any feature. Run the
[GPU validation](../gpu-validation.md) and [GPU-cluster runbook](../gpu-ai-runbook.md) on rented GPU nodes (one
8-GPU node is enough to start: a fractional job, a workspace, a vLLM service behind the LLM gateway) and publish the
results with the method, including what failed. Add one GPU job to CI if a runner can be had.

**Next: build on Run:ai's open components.**

1. KAI Scheduler as a scheduling backend, like the Kueue integration. **Done for jobs** (opt-in `--kai-scheduler`):
   pods go to KAI in a queue, multi-pod jobs are gang scheduled, `gryvia.io/gpu-share` becomes KAI's `gpu-fraction`;
   `scripts/e2e-kai.sh` passed on kind with a simulated GPU (see [KAI Scheduler integration](../kai-scheduler.md)).
   Still open: creating KAI queues from tenants, and copying KAI's reasons into job status.
2. GPU Fractioning Operator as the `cuda` isolation for `gryvia.io/gpu-share`: driver-enforced memory limits instead
   of MPS where the driver supports it; keep MIG and MPS for older drivers.
3. Map Gryvia tenants to KAI Resource Management projects and departments so metering and invoices follow the same
   hierarchy the scheduler uses.

**Then: deepen what nobody else bundles.** Live payments (beyond Stripe test mode), tenant self-service sign-up and
usage pages, per-token pricing in the SKU catalog for the LLM gateway, and the model factory running end to end on
GPUs.

**Not planned:** our own dynamic fractions, GPU memory swap or a new scheduler. Use Run:ai's.

## How we will know

Time from `git clone` to a running dashboard (target: under ten minutes on a laptop, measured in CI); a published GPU
validation report; an install that bills at least one real tenant; GitHub stars, issues and external contributors per
month.

## Risks

- Run:ai partners already sell billing and catalogs on top of it; Gryvia has to be the open, integrated option there.
- NVIDIA may add metering or an LLM gateway to Run:ai itself; the tenant economics would still need to be better.
- Breadth without proof reads as vapourware: the GPU proof comes before new features.
