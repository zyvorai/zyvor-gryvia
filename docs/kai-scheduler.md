# KAI Scheduler integration

[KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) is an open-source Kubernetes scheduler for AI
workloads, Apache 2.0 and a CNCF Sandbox project: hierarchical queues with quotas and limits, fair share, gang scheduling,
topology-aware placement, bin-packing and GPU sharing. With the integration on, Gryvia hands the pods of every
`GryviaAIJob` to KAI and keeps its own job lifecycle, tenants, metering and billing on top.

**Status:** opt-in, off by default. Unit-tested (`controllers/gryviaaijob_kai_test.go`). `scripts/e2e-kai.sh`
(workflow `.github/workflows/e2e-kai.yml`) passed from a clean install on a single-node kind cluster (Kubernetes 1.37,
KAI v0.18.5), with [fake-gpu-operator](https://github.com/run-ai/fake-gpu-operator) advertising one simulated GPU:

- a CPU job is placed by KAI in its queue and succeeds;
- three half-GPU jobs: two run at the same time with the same GPU, the third waits until one finishes;
- a 3-pod job gets one KAI PodGroup with `minMember` 3, starts no pod while the queue's CPU limit is too small for
  all three, and starts all three when the limit is raised.

The GPU is simulated: this shows placement, gang and sharing decisions, not CUDA on a real device or memory isolation.
Nothing has run on GPU hardware.

## Turn it on

Install KAI Scheduler (v0.18 or later), then Gryvia with the flag:

```bash
helm upgrade -i kai-scheduler oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler \
  -n kai-scheduler --create-namespace --version v0.18.5
helm upgrade --install gryvia ./helm/gryvia -n gryvia-system --set aiOperator.kaiScheduler=true
```

Pick the version from KAI's [releases](https://github.com/kai-scheduler/KAI-Scheduler/releases), not the registry's
tag list: the chart registry also holds an older `v0.20.1` (November 2025) that sorts higher. That build gives every
pod of a batch Job its own PodGroup, so multi-pod jobs are not gang scheduled.

KAI's own requirements, which `scripts/e2e-kai.sh` meets on kind:

- KAI's operator does not start the scheduler until the `clusterpolicies.nvidia.com` CRD exists (NVIDIA GPU Operator,
  or the fake-gpu-operator on clusters without GPUs).
- GPU fractions need KAI's GPU sharing (`global.gpuSharingMode`, default `NonMemoryEnforced`) and the node label
  `nvidia.com/gpu.memory` (MiB), which GPU feature discovery sets on real GPU nodes.

The ai-operator flags are `--kai-scheduler` and `--kai-default-queue` (chart `aiOperator.kaiDefaultQueue`, default
`default-queue`, which a fresh KAI install creates). The integration cannot be combined with `--kueue-integration` or
`--native-queue-scheduler`; the operator and the chart refuse it. It needs no extra RBAC: the operator reads and writes
no KAI object.

## What changes for a job

| | Without KAI | With `--kai-scheduler` |
|---|---|---|
| Placement | The operator ranks GPU nodes and leaves the job `Pending` when too few GPUs are free | Skipped: capacity is KAI's decision. Condition `Scheduled` says `Placement delegated to KAI Scheduler (queue …)` |
| Pod template | Default scheduler | `schedulerName: kai-scheduler`, label `kai.scheduler/queue` = `spec.queueName`, else the annotation `gryvia.io/queue-name`, else the default queue |
| Multi-pod batch Job | Pods start as they fit | Annotation `kai.scheduler/batch-min-member` = all pods (an elastic job's `minNodes`): KAI places them together or not at all |
| `gryvia.io/gpu-share` below 1 | Native queue scheduler only (MIG, MPS, DRA intent) | `gpu-fraction` (or `gpu-memory` when `gryvia.io/gpu-memory` is set) pod annotation, and no `nvidia.com/gpu` request |
| Inference (StatefulSet) | Default scheduler | Same scheduler name and queue label on the pod template; the selector is unchanged |

Queues, quotas and limits are KAI `Queue` objects (`scheduling.run.ai/v2`); only leaf queues take jobs. Priority:
KAI reads the pod's `priorityClassName`, which the job's annotation `gryvia.io/priority-class` sets; without one KAI
uses its default (`train`, preemptible). KAI's `build` and `inference` classes are non-preemptible and only run within
a queue's quota, which is 0 on the default queues.

GPU fractions: `gryvia.io/gpu-isolation` is not used. KAI's install-time `global.gpuSharingMode` decides enforcement:
`NonMemoryEnforced` (the default, no memory isolation), `HamiCore`, or `NvFractions` (CUDA-enforced memory limits
through the GPU Fractioning Operator). See KAI's [GPU sharing](https://github.com/kai-scheduler/KAI-Scheduler/tree/main/docs/gpu-sharing)
guide. A job with `spec.migProfile` keeps its MIG resource.

## Not done yet

- Tenants are not mapped to KAI queues: create a `Queue` per team and set `spec.queueName` (or `--queue` in
  `gryvia submit`). Creating them from `GryviaTenant` quotas is on the roadmap.
- The job phase stays `Scheduling` while KAI holds its pods; KAI's reason is on the PodGroup and the pods' events
  (`kubectl get podgroups -n <ns>`), not yet copied into the job status.
- `gryvia.io/gpu-share` above 1 (for example 1.5) and KAI's multi-GPU fractions are not mapped.
