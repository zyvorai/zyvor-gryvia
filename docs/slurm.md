# Slurm batch scripts

`gryvia submit --sbatch` runs a Slurm batch script as a `GryviaAIJob`. The `#SBATCH` directives become spec fields,
the script becomes the container command, and the `SLURM_*` variables a script usually reads are set in every pod.
There is no Slurm controller involved: the job is scheduled by Gryvia (and Kueue, when the integration is on) like
any other `GryviaAIJob`. For users who need Slurm itself (`sbatch`, `squeue`, `sacct`, job steps), the chart can also
run a real Slurm cluster on Kubernetes with a partition per tenant: see [Real Slurm (Slinky)](#real-slurm-slinky).

```bash
gryvia submit --sbatch train.sh --image pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime --dry-run   # print the YAML
gryvia submit --sbatch train.sh --image pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime --wait --logs
gryvia -n tenant-alpha submit --sbatch sweep.sh --image ghcr.io/acme/train:1.2
```

`--dry-run` prints the jobs as YAML on stdout and the warnings on stderr, so
`gryvia submit --sbatch train.sh --image ... --dry-run | kubectl apply -f -` works too. Slurm has no container image,
so pass `--image` or add `#SBATCH --container-image=<image>` (the pyxis directive; `nvcr.io#nvidia/pytorch:24.01-py3`
becomes `nvcr.io/nvidia/pytorch:24.01-py3`). `--image` wins when both are given.

## Directive mapping

| `#SBATCH` | GryviaAIJob |
| --- | --- |
| `-J`, `--job-name` | `metadata.name` (lower case, other characters become `-`); the original is `SLURM_JOB_NAME`. Without it, the file name |
| `-p`, `--partition` | `spec.queueName`, the Kueue LocalQueue (used only with the Kueue integration, see [Kueue integration](kueue-integration.md)) |
| `-N`, `--nodes=N` | `distributed: {enabled: true, nodes: N}`, one pod per node; CPU-only jobs get `backend: gloo` |
| `--nodes=min-max` | `distributed.nodes: max` with `elastic.minNodes: min` ([elastic training](elastic-training.md)) |
| `-n`, `--ntasks` without `--nodes` | one pod per task (`ceil(ntasks / ntasks-per-node)` pods) |
| `--gres=gpu[:type][:N]`, `--gpus-per-node=[type:]N` | `gpus: N` per pod, `gpuType` upper case (`a100`, `tesla_a100` become `A100`) |
| `-G`, `--gpus=[type:]N` | `gpus: ceil(N / nodes)` per pod |
| `--gpus-per-task=N` | `gpus: N × ntasks-per-node` |
| `-c`, `--cpus-per-task`, `--cpus-per-gpu` | `resources.requests.cpu` (per task × tasks per node, or per GPU × GPUs) |
| `--mem`, `--mem-per-cpu`, `--mem-per-gpu` | `resources.requests.memory` and `limits.memory` (Slurm's MB default; K/M/G/T become Ki/Mi/Gi/Ti) |
| `-t`, `--time` | `timeout` (`MM`, `MM:SS`, `HH:MM:SS`, `D-HH[:MM[:SS]]`; `UNLIMITED` and `0` set none) |
| `-a`, `--array=0-3,7,10-20:5` | one job per index, named `<name>-<index>`, labelled `gryvia.io/sbatch-array=<name>`, at most 256 |
| `-D`, `--chdir` | `workingDir` |
| `--container-image` | `image` (unless `--image` is given) |

Directive parsing stops at the first command line, like `sbatch`. Every job is labelled
`gryvia.io/imported-from: sbatch`.

These print a warning and are otherwise ignored: `--output`/`--error` (read the output with `gryvia logs`),
`--dependency` (chain jobs with a [GryviaWorkflow](../website/docs/guides/ML_WORKFLOWS.md)), `--signal` (pods get
SIGTERM on eviction; see the checkpoint hooks in [admission and recovery](admission-recovery.md)), `--exclusive`,
`--export`, an array's `%limit` (all indexes are submitted at once; a Kueue queue can hold them), `--gres` resources
other than GPUs, `--mem=0`, and any directive without an equivalent (`--account`, `--qos`, `--constraint`,
`--nodelist`, `--mail-*`, ...). A malformed value (`--nodes=two`, `--time=later`) is an error and nothing is
submitted.

## What the script sees

| Variable | Value |
| --- | --- |
| `SLURM_PROCID`, `SLURM_NODEID` | The pod index (`RANK` of a distributed job, else `JOB_COMPLETION_INDEX`) |
| `SLURM_LOCALID` | `0` |
| `SLURM_NTASKS`, `SLURM_NPROCS`, `SLURM_NNODES`, `SLURM_JOB_NUM_NODES` | The number of pods |
| `SLURM_NTASKS_PER_NODE`, `SLURM_TASKS_PER_NODE` | `1`, `1(x<pods>)` |
| `SLURM_JOB_ID` | The GryviaAIJob name (Slurm's is a number; scripts that only use it in file names are fine) |
| `SLURM_JOB_NAME`, `SLURM_JOB_PARTITION` | From the directives |
| `SLURM_ARRAY_TASK_ID`, `SLURM_ARRAY_JOB_ID`, `SLURM_ARRAY_TASK_COUNT`, `_MIN`, `_MAX` | Array jobs only |
| `SLURM_CPUS_PER_TASK`, `SLURM_CPUS_ON_NODE`, `SLURM_GPUS_ON_NODE`, `SLURM_MEM_PER_NODE` | When requested |
| `SLURM_JOB_NODELIST`, `SLURM_NODELIST` | The rendezvous host (pod 0); `MASTER_ADDR` and `MASTER_PORT` are set too |
| `SLURM_SUBMIT_DIR` | The working directory |

Two commands are defined as shell functions before the script runs:

- `srun [options] cmd ...` drops its options and runs `cmd` once in the pod. Each pod already is one task, so
  `srun python train.py` in a 4-node script runs `train.py` once on each of the 4 pods, as Slurm would with one task
  per node.
- `scontrol show hostnames` prints the rendezvous host, so the common
  `MASTER_ADDR=$(scontrol show hostnames "$SLURM_JOB_NODELIST" | head -n1)` gives pod 0. Other `scontrol` calls fail
  with a message.

The script runs with its own interpreter from the `#!` line (a POSIX shell such as `sh` or `bash`, or `python`;
others are refused). For a python script the variables are set by `/bin/sh` first, so the image needs `/bin/sh`.

## Limits

- One task per pod. With `--ntasks-per-node` above 1 (and not equal to the GPUs per node), start the processes
  yourself, for example `torchrun --nproc-per-node=$SLURM_GPUS_ON_NODE`; the importer warns.
- No job steps, `sacct`, `squeue`, `scancel`, heterogeneous jobs or `--dependency`. Use `gryvia list jobs`,
  `gryvia logs`, `gryvia cancel` and workflows.
- `SLURM_JOB_NODELIST` holds one host (pod 0), not every node.
- Partitions are not created; a partition name is only used as the Kueue LocalQueue name. (With
  [real Slurm](#real-slurm-slinky), each tenant has a partition; submit there with Slurm's own `sbatch`.)

## What is verified

| Verified | How |
| --- | --- |
| Each directive's mapping, time, memory, GPU and array formats, name sanitizing, warnings for ignored and unknown directives, errors for malformed values, interpreters, and the shims run under `/bin/sh` (`SLURM_PROCID` from `RANK` or `JOB_COMPLETION_INDEX`, `srun` option stripping, `scontrol show hostnames`) | Unit tests in `cli/src/commands/sbatch.rs` |
| A 2-node busybox script submitted with `gryvia submit --sbatch --wait` on kind: it Succeeds, the logs show `SLURM_PROCID` 0 and 1, `SLURM_NTASKS=2`, `srun` running once per pod and `scontrol show hostnames` returning pod 0; a 2-index array runs `sweep-0` and `sweep-1` with their `SLURM_ARRAY_*` values | `scripts/e2e-jobs.sh sbatch` in `.github/workflows/e2e-jobs.yml` |

Not verified: GPU scripts (CI has no GPU) and real Slurm job scripts from a site. This importer does not run Slurm;
the jobs never reach a Slurm controller.

## Real Slurm (Slinky)

The chart can run SchedMD's [Slinky](https://slinky.schedmd.com/) slurm-operator and a Slurm cluster (Slurm 26.05),
and give every `GryviaTenant` its own partition. Users then submit with the real `sbatch`, and finished jobs are
metered into `GryviaUsageRecord`s, so budgets and chargeback include Slurm.

Install in two steps: one release cannot create the operator's CRDs and the Slurm objects together.

```bash
# 1. The slurm-operator (sub-chart slurm-operator 1.2.3) and its CRDs under slinky.slurm.net.
helm upgrade gryvia ./helm/gryvia -n gryvia-system --reuse-values --set slurmOperator.enabled=true --wait
# 2. The Slurm cluster and the per-tenant partitions.
helm upgrade gryvia ./helm/gryvia -n gryvia-system --reuse-values \
  --set slurm.enabled=true --set quotaOperator.slurmIntegration=true --wait
```

Run `helm dependency build helm/gryvia` first when installing from a checkout. With `slurm.enabled` and no
`slinky.slurm.net` CRDs on the cluster, the chart fails with a message instead of half-installing.

| Object | What it is |
| --- | --- |
| Deployments `slurm-operator`, `slurm-operator-webhook` | Slinky's operator and webhook (the webhook's certificate is generated by the chart, no cert-manager) |
| Secrets `slurm-auth-slurm`, `slurm-auth-jwt` | Slurm's auth key and JWT key, generated once and kept across upgrades |
| Controller `slurm` (pod `slurm-controller-0`) | slurmctld, with its state on a 4 Gi PVC (`slurm.persistence`) |
| RestApi `slurm` (Service `slurm-restapi:6820`) | slurmrestd |
| Token `gryvia-accounting` (Secret `gryvia-slurm-token`) | A JWT for user `slurm`, rotated by the operator, that the quota operator sends to slurmrestd |
| NodeSet `gryvia-<tenant>` per GryviaTenant | slurmd pods `gryvia-<tenant>-0`, ..., and the partition `gryvia-<tenant>` |

The Slurm objects are templated by this chart (`templates/slurm-cluster.yaml`) with the defaults of Slinky's own
`slurm` chart 1.2.3, not pulled in as that chart: it and the operator chart define the same template helper, so they
cannot both be sub-charts of one chart. There are no login nodes; run `sbatch` where the Slurm client commands are,
for example `kubectl -n gryvia-system exec slurm-controller-0 -c slurmctld -- sbatch -p gryvia-<tenant> job.sh`.

### Tenant partitions

With `quotaOperator.slurmIntegration` the quota operator (flag `--slurm-integration`) keeps a NodeSet per tenant,
labelled `gryvia.io/tenant`, next to the Controller. Slinky names the partition after the NodeSet:

- **Nodes.** `quotaOperator.slurm.gpusPerNode` (default 0) is the `nvidia.com/gpu` limit of every slurmd pod. When it
  is set, the tenant's GPU quota (`spec.quotas.concurrentGPUs`, else the `maxGPUs` of a GryviaQuota listing
  `tenant-<name>`) divided by it, rounded up, is the node count; otherwise `nodesPerTenant` (default 1). The
  annotation `gryvia.io/quota-source` says which (tenant, quota or default).
- **Limits.** The partition line is `MaxNodes=<nodes>`.
- Deleting the tenant deletes its NodeSet (finalizer `gryvia.io/slurm-finalizer`). A NodeSet with that name that the
  quota operator did not create is left alone.

### Accounting

Every `accountingInterval` (30 s) the quota operator reads slurmctld's job list from slurmrestd
(`GET /slurm/v0.0.44/jobs`, falling back to v0.0.43) and writes one final record per finished job (COMPLETED,
FAILED, CANCELLED, TIMEOUT, NODE_FAIL, OUT_OF_MEMORY, BOOT_FAIL, DEADLINE, PREEMPTED) in a tenant partition:

- Name `slurm-<cluster>-<job id>` in `tenant-<tenant>`, `spec.kind: slurm`, labels `gryvia.io/tenant` and
  `gryvia.io/usage-kind: slurm`, `spec.job: slurm:<job name>`.
- Start and end from Slurm; `gpus` and `gpuType` from the job's allocated TRES (`gres/gpu[:type]=N`); `gpuHours`,
  `rate` and `cost` priced like GryviaAIJobs (the matching GryviaGpuSku). CPU-only jobs cost 0.
- Annotations `gryvia.io/slurm-job-id`, `-partition`, `-user`, `-account`, `-state` and `-tres`.

A finished job stays in slurmctld's list for `MinJobAge` (300 s by default). If the quota operator is down longer than
that, the jobs that finished meanwhile are not recorded; there is no slurmdbd in this setup. Records are created once
per job, so a restart or a leader change does not count a job twice.

### Using your own Slurm

Leave `slurm.enabled` off and point the quota operator at an existing Slinky Controller: `quotaOperator.slurm.namespace`,
`controller`, `restapiUrl`, and `tokenSecret`/`tokenKey` (a Secret in that namespace holding a slurmrestd JWT; empty
turns accounting off).

### What is verified

| Verified | How |
| --- | --- |
| NodeSet per tenant (controller reference, replicas, partition line, labels, finalizer), node count from the GPU quota, updates, deletion with the tenant, unmanaged NodeSets left alone; accounting of finished jobs only, once each, GPU count and type from TRES, SKU pricing, the API version fallback, token errors | `operators/quota-operator/controllers/gryviaslurm_controller_test.go` (fake client, httptest slurmrestd) |
| On kind (CPU): the operator sub-chart and CRDs, the Slurm cluster, the guard without CRDs, a tenant's NodeSet and partition (`MaxNodes=1`) with an idle node, `sbatch` of a 2-task job that completes with both tasks' output, its usage record (kind slurm, final, state COMPLETED) written once, and the NodeSet removed with the tenant | `.github/workflows/e2e-slurm.yml` |

Not verified: GPUs in Slurm (no GPU in CI), more than one node per partition, slurmdbd accounting, and login nodes.

