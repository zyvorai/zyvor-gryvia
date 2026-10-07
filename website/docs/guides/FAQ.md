# Frequently Asked Questions

Answers about what Gryvia does today. Where a feature has a CRD but no controller, or has not been verified on real hardware, the answer says so. See the [roadmap](ROADMAP.md) and the [CRD reference](../reference/crds.md) (which lists which kinds have a controller).

## General

### What is Gryvia?

An alpha, Kubernetes-native platform for GPU workloads. What runs today:

- A `GryviaAIJob` operator that selects GPU nodes and creates the StatefulSet, Service and PVC for a training or inference job
- GPU node registration (`GryviaGpuNode`) from NVIDIA GPU feature discovery labels
- Multi-tenancy: tenants, quotas, a GPU price catalog, per-job usage metering and estimate invoices (opt-in: a hash-chained billing ledger, finalized invoices and Stripe test mode)
- An admission webhook that checks jobs against quota and SKU policy
- Storage and network operators, and network intelligence (eBPF collector, off by default)
- A REST API gateway, a dashboard and a Rust CLI, plus Python and Go SDKs

Workflows, tuners, workspaces, inference services and the model registry have controllers in the ai-operator (unit-tested; the kind e2e with tiny CPU images passes in CI; nothing on GPUs), budgets, chargeback and reservations have controllers in the quota-operator (reservations are opt-in), and Kueue-based queueing is opt-in. Of the 49 kinds, 43 have a controller (several opt-in); the other six are catalog, telemetry or billing-ledger data that controllers and the gateway write and read. The old auto-scaling, SLA and similar kinds without a runtime were removed.

### Who should use it?

Teams that want to experiment with a GPU platform on Kubernetes and accept alpha software. The API is `gryvia.io/v1alpha1` and can change. Nothing has been verified on real GPU or RDMA hardware by this project's CI.

### How is Gryvia different from Kubeflow?

They overlap little today. Kubeflow provides ML pipelines, notebooks and serving that run. Gryvia's DAG workflow, tuning, serving and notebook kinds are CRDs without controllers, so use Kubeflow (or another engine) for those. Gryvia's running parts are the job operator, tenancy and quota, metering, and GPU node and network tooling. They can be installed side by side; that combination has not been tested here.

### Is Gryvia a GPU cloud?

No. It is the platform layer a GPU cloud runs on its own clusters: tenants, quotas, queueing, metering, inference
endpoints, model APIs and fine-tuning. The provider brings the hardware, facilities and machine provisioning.
[GPU cloud platform](https://github.com/zyvorai/gryvia/blob/main/docs/gpu-cloud-platform.md) maps what GPU clouds
advertise to what Gryvia has and how each part is tested.

---

## Getting Started

### How do I submit my first job?

Install the chart, then submit a `GryviaAIJob` manifest with the CLI or `kubectl`:

```bash
helm install gryvia oci://ghcr.io/zyvorai/charts/gryvia \
  --namespace gryvia-system --create-namespace \
  --set auth.apiKey='a-long-random-secret'

gryvia validate job.yaml
gryvia submit --file job.yaml
gryvia status my-job
gryvia logs my-job --follow
```

A minimal job (required fields are `type`, `gpus` and `image`):

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: my-job
spec:
  type: training
  gpus: 8
  gpuType: A100-80G
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command: ["python", "train.py"]
```

The CLI talks to the Kubernetes API through your kubeconfig, not through the gateway. See the [job guide](../user-guide/jobs.md) and [CLI guide](CLI_GUIDE.md).

### What GPU types are supported?

Gryvia does not restrict GPU models. `spec.gpuType` is matched against the `gryvia.io/gpu` node label (values are whatever your nodes carry, for example the names in your `GryviaGpuSku` catalog), and pods request `nvidia.com/gpu`, so any NVIDIA GPU with a working device plugin can be scheduled. Which models have actually been run is not recorded: no GPU hardware runs in this project's CI. MIG and fractional sharing: NVIDIA's GPU Operator does the partitioning; the opt-in `GryviaGPUSharingPolicy` controller (`gpuOperator.gpuSharing`) only labels the nodes it should configure (see [NVIDIA one-click](NVIDIA_ONE_CLICK.md)).

### Can I use my existing Kubernetes cluster?

Yes, the chart installs into any cluster. On a bare GPU server, `scripts/install-k3s-gpu.sh` can set up k3s, Gryvia and NVIDIA's GPU Operator. See [GPU nodes](GPU_NODES.md) and the [deployment guide](DEPLOYMENT_GUIDE.md).

---

## Resource Management

### How do quotas work?

`GryviaQuota` objects (reconciled by the quota-operator) describe limits and usage per namespace, and `GryviaTenant` gives a team a `tenant-<name>` namespace with a ResourceQuota. At job creation the admission webhook denies jobs whose GPU type is not allowed by the namespace's quotas or the tenant's allowed SKUs, or that exceed the per-job GPU limit. The webhook fails open (jobs are admitted if it is unreachable, by default), and it does not check monthly GPU-hour or cost limits.

```bash
gryvia quota my-team
gryvia quota my-team --budget
```

### Can I request more quota?

There is no request workflow. A platform admin edits the `GryviaQuota` or `GryviaTenant`, or you use `gryvia tenant create` to update a tenant.

### What happens if I exceed my budget?

By default nothing blocks you: `GryviaBudget` and the `GryviaQuota` budget are computed and shown in status, but jobs are only rejected when the opt-in admission gate is on (`aiOperator.admissionGate`), and then only on estimated spend and only for jobs not yet started; the gate fails open when it cannot look up spend, and there are no notifications or throttling. `gryvia cost` and `gryvia usage` show estimated spend from metered usage and the SKU catalog prices; they are estimates, and Gryvia does no billing or payment processing.

---

## Cost

### How can I reduce costs?

Gryvia gives you visibility, not automatic savings:

- `gryvia capacity` shows free GPUs, pending demand and shortfall per type
- `gryvia cost` and `gryvia usage` show estimated spend
- The catalog (`gryvia catalog`) shows the hourly rates used for estimates

Spot handling, auto-scaling, reservations and discounts are CRDs without controllers, so they do not reduce anything. Savings figures that older versions of this page quoted (spot percentages, MIG percentages, reservation discounts) were illustrative and unmeasured.

### What is MIG and when should I use it?

MIG (Multi-Instance GPU) partitions A100/H100-class GPUs into isolated instances. It suits small models, development and inference; it is not suited to large distributed training. Gryvia does not manage MIG itself: use NVIDIA's GPU Operator to configure it and expose the MIG resources, then request them in your pod resources. Unverified with Gryvia on hardware.

---

## Job Management

### How do I retry failed jobs?

Training, fine-tuning and evaluation jobs run as a batch Job: `spec.retryLimit` is its `backoffLimit` (default 0, so the first failed pod fails the job) and `status.retries` counts failed pods. There is no `retryPolicy` field on the job. Inference jobs (StatefulSet) restart a crashed container in place. A `Failed` job stays failed; to rerun, delete and resubmit:

```bash
gryvia delete job my-job --yes
gryvia submit --file job.yaml
```

### Can I checkpoint and resume jobs?

Your training code does the checkpointing. Write to a volume that survives the pod, using `spec.storage` (PVC mounted at `/data`) or your own `volumes`, and have the code resume from it on start. There is no `checkpointing` block on `GryviaAIJob` and Gryvia does not resume jobs on preemption or spot interruption. With the opt-in `aiOperator.checkpointGuard`, a `GryviaCheckpointGuard` tells the trainer where and how often to save, records the steps it commits and replaces the pods of a non-elastic job whose node is lost; see [checkpoint guard](https://github.com/zyvorai/gryvia/blob/main/docs/checkpoint-guard.md).

### How do I run distributed training?

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: ddp-4x8
spec:
  type: training
  image: nvcr.io/nvidia/pytorch:24.01-py3
  gpus: 8
  gpuType: A100-80G
  distributed:
    enabled: true
    framework: pytorch
    backend: nccl
    nodes: 4
    gpusPerNode: 8
  command: ["torchrun"]
  args: ["--nproc_per_node=8", "--nnodes=4", "--master_addr=$(MASTER_ADDR)", "--master_port=$(MASTER_PORT)", "train.py"]
```

The controller creates an Indexed Job with 4 pods of 8 GPUs each and sets `MASTER_ADDR`, `MASTER_PORT`, `WORLD_SIZE` (`nodes * gpusPerNode`), `RANK`/`NODE_RANK` (the pod index) and `NCCL_DEBUG`; with `network: rdma` it adds `NCCL_IB_DISABLE=0`, `NCCL_NET_GDR_LEVEL=5` and RDMA annotations. Pods are placed by the Kubernetes scheduler, without gang scheduling unless the opt-in Kueue integration is on (then Kueue admits all pods together; see [Kueue integration](https://github.com/zyvorai/gryvia/blob/main/docs/kueue-integration.md)). See [Scheduling](SCHEDULING.md).

---

## Performance

### My training is slow. How do I optimize?

Start with measurements:

```bash
gryvia gpu training --job my-job
gryvia gpu nccl --job my-job
gryvia health gpu
```

`gryvia gpu` reads data from the network-intelligence collector, which is off by default and unverified on real GPUs. Common ML advice applies (mixed precision, batch size, data loading, `torch.compile`); it is not something Gryvia automates. Operational steps are in [OPERATIONAL_PLAYBOOKS.md](OPERATIONAL_PLAYBOOKS.md).

### How do I enable GPUDirect RDMA?

Set `network: rdma` on the job. The controller then selects nodes labelled `gryvia.io/rdma=true`, adds the `gryvia.io/rdma` and `rdma-network` annotations and the two NCCL variables above. It requires RDMA-capable NICs, the NVIDIA driver stack with GPUDirect support and a network attachment named `rdma-network` in the namespace; none of that has been verified by this project on hardware.

### What is good GPU utilization?

That depends on the workload; Gryvia sets no target. Use the dashboard, the DCGM exporter metrics or `gryvia status` to look at your own jobs.

---

## Multi-Tenancy

### How do I create a team?

```bash
gryvia tenant create ml-research \
  --display-name "ML Research Team" \
  --allowed-sku a100-80g \
  --max-gpus 128 \
  --isolated true
gryvia tenant list
```

The tenant's workloads run in the namespace `tenant-ml-research`. See [GPU as a service](GPU_AS_A_SERVICE.md).

### How do I add team members?

`GryviaTenant.spec.members` records usernames and roles (`admin`, `member`, `viewer`) but the controller does not enforce them. Access in the gateway comes from authentication: the API key or session is a provider admin, and OIDC users are tenant users whose tenant is taken from the `org` or `groups` claim. Kubernetes RBAC for `kubectl` users is yours to configure. See [Auth and TLS](AUTH_AND_TLS.md).

### Can teams share GPUs?

Only through Kubernetes and NVIDIA mechanisms (the device plugin's time-slicing, MIG). With `gpuOperator.gpuSharing` a `GryviaGPUSharingPolicy` labels the matching GPU nodes with the device-plugin or MIG configuration NVIDIA's components then apply; whether the device plugin advertises the shared GPUs has not been checked on a GPU node. See [NVIDIA one-click](NVIDIA_ONE_CLICK.md).

---

## Security and Compliance

### Is Gryvia SOC2 or HIPAA compliant?

No. Gryvia has no compliance certification, and it does not implement an audit trail of its own (use the Kubernetes API server audit log and the gateway's request logs). Whether your deployment meets a framework depends on your cluster, storage, identity provider and processes. See [SECURITY.md](https://github.com/zyvorai/gryvia/blob/main/SECURITY.md) and [Auth and TLS](AUTH_AND_TLS.md) for what the project does provide (signed sessions, rate-limited login, tenant scoping, TLS on the gateway and dashboard).

### How is data encrypted?

Gryvia does not encrypt your data itself. The gateway serves HTTPS with the chart's certificate (self-signed by default; cert-manager is optional). Encryption at rest for volumes and Kubernetes Secrets is whatever your cluster and storage provider give you.

---

## Other Features

### What are job hooks?

A `GryviaJobHook` sends a signed HTTP POST when a `GryviaAIJob` or `GryviaWorkflow` in its namespace reaches a chosen phase (for example `Failed` or `Succeeded`). It is opt-in (`aiOperator.jobHooks.enabled`); see [job hooks](https://github.com/zyvorai/gryvia/blob/main/docs/job-hooks.md).

### Does auto-scaling work?

Gryvia does not add or remove nodes. Use your cloud's cluster autoscaler or Karpenter; inference services scale their pods with an HPA.

### Can I reserve GPUs in advance?

Yes, opt-in. With `quotaOperator.reservations` a `GryviaReservation` taints and labels the reserved nodes for its time window, and jobs annotated `gryvia.io/reservation: <name>` tolerate the taint (`gryvia reservation create`). Unit-tested and the kind e2e passes in CI; never run on GPUs. See [GPUaaS completion](https://github.com/zyvorai/gryvia/blob/main/docs/gpuaas-completion.md).

---

## Troubleshooting

### My job is stuck in Pending

When node selection fails the controller sets the `Scheduled` condition with a message such as `no nodes meet the job requirements (gpuType="H100", gpus=8, network="rdma", 12 nodes evaluated)` and retries after 30 seconds.

```bash
# 1. Why? Read the conditions
kubectl describe gryviaaijob my-job

# 2. Jobs waiting, and free GPUs
gryvia queue
gryvia capacity
gryvia list nodes

# 3. Quota
gryvia quota my-team
```

Check that nodes carry the labels the job needs (`gryvia.io/gpu`, `gryvia.io/gpu-count` or allocatable `nvidia.com/gpu`, `gryvia.io/rdma=true` for RDMA jobs). If the pods exist but stay unscheduled, look at the pod events: placement is done by the Kubernetes scheduler.

### Jobs keep failing

```bash
gryvia logs my-job --tail 100
kubectl describe gryviaaijob my-job
kubectl get pods -l gryvia.io/job=my-job
```

Typical causes: out-of-memory, image pull errors, a missing volume or secret, an unhealthy GPU node (`gryvia health gpu`).

### How do I get support?

1. **Documentation**: this site
2. **GitHub Issues**: https://github.com/zyvorai/gryvia/issues
3. **GitHub Discussions**: https://github.com/zyvorai/gryvia/discussions

---

## Best Practices

```yaml
# Reasonable
spec:
  type: training
  image: nvcr.io/nvidia/pytorch:24.01-py3
  gpus: 8
  gpuType: A100-80G
  resources:
    requests:
      cpu: "64"
      memory: 512Gi
  storage: fast-nfs        # a GryviaStorage backend name; PVC mounted at /data
  storageRequest: 500Gi
```

1. Label jobs with team and project so usage can be grouped.
2. Request realistic CPU and memory; do not over-request GPUs.
3. Checkpoint from your training code to a persistent volume.
4. Set quotas for tenants; budgets are not enforced.
5. Profile before optimizing.

---

## Migration

### From Slurm

`gryvia submit --sbatch job.sh --image <image>` imports a batch script: `#SBATCH` directives become `GryviaAIJob` fields (`--gres=gpu:8` becomes `gpus: 8`, `--nodes=4` a 4-node distributed job, `--array` one job per index), the script runs as the command, and `SLURM_PROCID`, `SLURM_NTASKS`, `srun` and `scontrol show hostnames` work inside it. Add `--dry-run` to see the YAML first. Directives without an equivalent are reported and ignored. See [Slurm batch scripts](https://github.com/zyvorai/gryvia/blob/main/docs/slurm.md) for the mapping and its limits (one task per pod, no job steps). If you need Slurm itself, the chart can also run a real Slurm cluster with SchedMD's Slinky operator and a partition per tenant, metered like any other job; see [Real Slurm (Slinky)](https://github.com/zyvorai/gryvia/blob/main/docs/slurm.md#real-slurm-slinky).

### From Kubernetes Jobs

There is no importer either. Write a `GryviaAIJob` with the same image and command; the controller wraps it in a StatefulSet.

---

## Limits

The documented limits of older versions of this page (GPUs per job, job duration, API rate limits, dataset sizes) were not enforced or measured. What the code actually enforces: `spec.gpus` must be greater than 0, `spec.priority` between 0 and 100, and a distributed job's total GPUs (nodes times GPUs per node) at most 1024 (webhook). The gateway rate-limits login attempts per client address.

---

## Glossary

- **MIG**: Multi-Instance GPU
- **GPUDirect**: NVIDIA peer-to-peer GPU communication
- **NCCL**: NVIDIA Collective Communications Library
- **DDP**: Distributed Data Parallel (PyTorch)
- **RDMA**: Remote Direct Memory Access
- **CRD**: Custom Resource Definition

---

*See [ADVANCED_FEATURES.md](ADVANCED_FEATURES.md) for the status of the advanced kinds.*
