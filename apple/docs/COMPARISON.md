# Where Gryvia fits

Reviewed 14 October 2026 from each project's public documentation and releases. These projects move quickly: treat the
"does not" column as "not what the project is for", and check the project before quoting it. Entries marked ⚠ were
written from general knowledge and should be re-verified before you rely on them.

| Project | What it is for | Relationship to Gryvia |
|---|---|---|
| **exo** | Discovers Macs and splits one model across them (MLX distributed; RDMA over Thunderbolt 5 on macOS 26.2+). | Different question: how to run *one* model on *many* Macs. Gryvia schedules whole jobs across a fleet under quotas. A future executor integration could let Gryvia place exo/mlx.launch jobs. |
| **MLX distributed / `mlx.launch`** | Primitives for multi-Mac collectives and distributed fine-tuning. | A launcher library. Gryvia could be the scheduler that decides which Macs such a job gets. |
| **llama.cpp RPC** | Offload layers of one model to remote ggml backends. | Same as above; Gryvia drives llama.cpp locally (bench, serve, quantise). |
| **Ollama, LM Studio** | Simple single-machine model management and serving. | Gryvia can sit above machines that each run such a server. |
| **vllm-metal** | vLLM scheduler and paged KV cache on MLX/Metal, inside one node. | Engine-level batching; Gryvia is fleet-level admission. |
| **GPUStack** ⚠ | Multi-node GPU cluster manager with model placement. | Mac/Metal worker support should be re-verified against its current docs. |
| **MacStadium Orka, AWS EC2 Mac** | Mac VMs and bare metal for CI and build farms. | Provisioning, not AI scheduling; Metal GPU access inside VMs is limited. |
| **Kubernetes, Nomad, Slurm** | General schedulers. | Do not model unified memory, GPU cores or owner presence for macOS; you would build Gryvia's admission logic on top. |
| **Petals, Ray** ⚠ | WAN swarm inference (CUDA/PyTorch); general distributed Python. | Not Apple-GPU-aware admission. |

## What is distinctive, in combination

- A native Swift agent per Mac plus a control plane with **memory-reservation admission against unified memory**, tenant quotas and fairness, for **mixed workload classes** (inference, MLX fine-tuning, llama.cpp benchmarks, onboarding, evaluation, deployment) in one queue.
- **Owner awareness** for desktop Macs: idle-time harvesting with instant yield and resume.
- A **live, measured GPU view** across the fleet, wired to scheduling decisions.
- **Fleet-internal signed provenance** across download → fine-tune → fuse.

## What we do not claim

No "first" or "only". Individually, durable leases, Prometheus metrics, web and SwiftUI clients and per-box GPU monitors
(asitop, macmon) are common. Measured numbers in this repository are single runs on specific machines.
