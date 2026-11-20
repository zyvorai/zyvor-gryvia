# Sovereign AI OS pack for Zyntra

Gryvia's tenants, GPU nodes, AI jobs, models, inference services and datasets become Zyntra objects. Gryvia's
cluster stats become KPIs, and approved changes are Gryvia custom resources. The `sovereign-aios` chart mounts this
pack into Zyntra and gives it the Gryvia gateway URL, API key and certificate.

## The gaps this pack is judged on

| Gap | Owner | KPI | Target |
| --- | --- | --- | --- |
| Inference waits for GPUs | ml-serving | `gpu_queue_wait_min` | at most 10 min |
| Jobs queue up | ml-platform | `gryvia_pending_jobs` | at most 2 |
| GPUs sit idle | ml-platform | `gpu_utilization` | at least 60% |
| The datapath is unhealthy | network | `ebpf_health_score` | at least 80 |

`gryvia_available_gpus` must not get worse: no action that trades away free GPUs is ranked.

`gpu_queue_wait_min` has no Gryvia source yet; it moves only through its edges from pending jobs and utilization.

## Actions

| Action | Runs as | Notes |
| --- | --- | --- |
| `raise-inference-priority` | `GryviaPriority` | Rollback deletes the object; outcome judged over 20 minutes |
| `enable-mig-sharing` | `GryviaGPUSharingPolicy` | Two approvals; rollback deletes the policy |

Zyntra runs these with `kubectl` (with `--dry-run=server` in the default dry-run mode), and its container image does
not carry `kubectl`. In the chart a proposal shows the rendered object for approval, but running it fails until
`kubectl` is provided (a sidecar or the binary install).

## Try it

```sh
zyntra pack validate helm/sovereign-aios/files/zyntra-pack
zyntra plan -f helm/sovereign-aios/files/zyntra-pack
```
