# Scheduling examples

Prefer live backends over the older annotation sketches in this folder:

- **KAI:** `scripts/e2e-kai.sh`, `docs/kai-scheduler.md`, `examples/kai/nested-tenants.yaml`
- **Kueue:** `docs/kueue-integration.md`, chart `aiOperator.kueueIntegration`
- **Native queues:** `deploy/native-scheduler-config.yaml`, `docs` / `NATIVE_SCHEDULER` guide


# Scheduling Examples

Example YAML manifests for Gryvia GPU scheduling CRDs covering elastic training, gang scheduling, and priority-based preemption.

> **Status.** Design examples. `GryviaPriority` has no registered controller. The `gryvia.io/elastic*` annotations are not implemented, so they have no effect today. The opt-in native queue gate is the in-tree gang/queue path: see [docs/native-scheduler.md](../../docs/native-scheduler.md) and `deploy/native-scheduler-config.yaml`.
>
> Real gang admission, queues and priority preemption exist only through the opt-in Kueue integration (`--kueue-integration`): see [docs/kueue-integration.md](../../docs/kueue-integration.md). The manifests below do not use it.

## Examples

- **[elastic-job-example.yaml](elastic-job-example.yaml)** - the real schema: `distributed.elastic.minNodes` (see docs/elastic-training.md)
- **[elastic-training.yaml](elastic-training.yaml)** (old design example, annotations not implemented) - `GryviaAIJob` with elastic annotations scaling between 2 and 8 nodes
- **[gang-scheduling.yaml](gang-scheduling.yaml)** - `GryviaAIJob` distributed job requiring atomic allocation of all 4 nodes
- **[priority-preemption.yaml](priority-preemption.yaml)** - `GryviaPriority` defining critical, normal, and preemptible scheduling tiers

## Usage

```bash
# Apply priority classes first (cluster-scoped)
kubectl apply -f priority-preemption.yaml

# Then submit jobs that reference those priorities
kubectl apply -f elastic-training.yaml
kubectl apply -f gang-scheduling.yaml
```

## Adult platform (vs Run:ai)

```bash
helm upgrade --install gryvia ./helm/gryvia -n gryvia-system \
  -f helm/gryvia/values-adult-platform.yaml
gryvia compare runai
```
