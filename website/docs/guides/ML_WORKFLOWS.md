# ML Workflows Guide

Gryvia's machine learning workflow kinds: hyperparameter tuning, DAG pipelines, model registry, inference serving and interactive workspaces, plus the training-side kinds that do have a running controller.

## Status: read this first

The ai-operator registers a controller for each of the five kinds this guide is mostly about (on by default; `--enable-ml-controllers=false` turns them off). What they do, their status fields, flags and RBAC are in [docs/ml-controllers.md](https://github.com/zyvorai/gryvia/blob/main/docs/ml-controllers.md). **Verification is limited:** unit tests with a fake client, and a kind workflow (`.github/workflows/e2e-ml.yml`, tiny CPU images in place of Jupyter, vLLM and Triton) that passes in CI. Nothing has run on GPUs, with a real model server image or with an HPA on real metrics.

| Kind | CRD | Gateway/dashboard | Controller registered |
|------|-----|-------------------|-----------------------|
| `GryviaAutoTuner` | yes | create, list, delete, list trials | yes (trials are child `GryviaAIJob`s) |
| `GryviaWorkflow` | yes | create, list, delete | yes (job, script and webhook steps; webhook steps off unless `--workflow-allow-webhooks`) |
| `GryviaModelRegistry` | yes | list, get, register (`POST /api/models`), promote | yes (`autoServe` at stage `production` creates a `GryviaInferenceService`) |
| `GryviaInferenceService` | yes | create, list, delete | yes (Deployment, Service, CPU-based HPA, pod-count canary) |
| `GryviaWorkspace` | yes | create, list, delete, pause and resume | yes (Pod, Service, optional PVC) |
| `GryviaModelWatch` | yes | create, list, delete, runs, suspend and resume | opt-in (`aiOperator.modelWatch.enabled`): one workflow per new hub model, see [Model factory](#model-factory-gryviamodelwatch) |
| `GryviaDataset` | yes | create, list, get, delete | opt-in (`storageOperator.datasets.enabled`, storage-operator): versions downloaded into a PVC, see [Datasets, LLM gateway, RAG and agents](#datasets-llm-gateway-rag-and-agents) |
| `GryviaVectorIndex` | yes | create, list, delete, reingest, suspend and resume | opt-in (`aiOperator.rag.enabled`): see [Datasets, LLM gateway, RAG and agents](#datasets-llm-gateway-rag-and-agents) |
| `GryviaAgent` | yes | create, list, delete, scale, chat | opt-in (`aiOperator.agents.enabled`): see [Datasets, LLM gateway, RAG and agents](#datasets-llm-gateway-rag-and-agents) |

Job steps and tuner trials create `GryviaAIJob`s, which the AIJob controller runs to completion as Indexed batch Jobs ([AIJob lifecycle](https://github.com/zyvorai/gryvia/blob/main/docs/aijob-lifecycle.md)). The sections below keep the schema examples; where a paragraph is still labelled design, the behaviour it describes is not implemented (for example weighted canary routing, or an HPA on GPU utilisation or requests per second). All YAML uses the real schema (`crds/`) and is checked in CI; the examples in `examples/ml-workflow/` are the source models.

The API routes are in the [API reference](../developer-guide/api-reference.md). See the [CRD reference](../reference/crds.md) for the Controller column across all kinds.

## Table of Contents

0. [What runs today](#what-runs-today)
1. [Hyperparameter Tuning (GryviaAutoTuner)](#hyperparameter-tuning-gryviaautotuner)
2. [DAG-Based Pipelines (GryviaWorkflow)](#dag-based-pipelines-gryviaworkflow)
3. [Model Registry (GryviaModelRegistry)](#model-registry-gryviamodelregistry)
4. [Inference Serving (GryviaInferenceService)](#inference-serving-gryviainferenceservice)
5. [Interactive Workspaces (GryviaWorkspace)](#interactive-workspaces-gryviaworkspace)
6. [Model factory (GryviaModelWatch)](#model-factory-gryviamodelwatch)
7. [Datasets, LLM gateway, RAG and agents](#datasets-llm-gateway-rag-and-agents)

---

## What runs today

The ai-operator registers controllers for these training-related kinds (examples in `examples/training/`):

- `GryviaAIJob`: creates the Indexed Job (or StatefulSet for inference), Service and PVC for a job (see [Scheduling](SCHEDULING.md#what-runs-today) and the [job guide](../user-guide/jobs.md)).
- `GryviaCheckpointGuard`: checkpoint protection for a job.
- `GryviaTrainingTimeMachine`: checkpoint history and forking experiments from checkpoints.
- `GryviaLiveExperiment`: compares experiment runs using metrics parsed from job logs (metric patterns are configurable).
- `GryviaTrainingProfiler`: training profiling.
- `GryviaModelLineage`: records provenance of a model.

The gpu-operator adds `GryviaGpuMemoryOptimizer`. All of these are exercised by Go unit tests against fake clients; none has been verified end to end with real GPU training runs.

---

## Hyperparameter Tuning (GryviaAutoTuner)

Status: the ai-operator runs the study: each trial is a child `GryviaAIJob` (`<tuner>-trial-<n>`), capped by `--tuner-max-trials` and `--tuner-max-parallelism`. A trial's metric is read from the annotation `gryvia.io/metric-<name>` that your training code (or whatever runs the trial) must set; unit-tested, the kind e2e (CPU pods) passes in CI, not verified on GPUs. See [docs/ml-controllers.md](https://github.com/zyvorai/gryvia/blob/main/docs/ml-controllers.md).

### Overview

`GryviaAutoTuner` describes a study: a search algorithm, an objective, a parameter space and a `GryviaAIJob` spec (`jobTemplate`) to run for each trial. The schema names four `searchAlgorithm` values: `grid`, `random`, `bayesian` and `asha` (with `ashaConfig` for `maxEpochs`, `minResource` and `reductionFactor`). The controller generates trials with the search library in `operators/ai-operator/pkg/tuner/search.go` (the Bayesian strategy is a simplified TPE-style heuristic). Do not rely on any convergence claim for these strategies; none has been measured here.

### Example (schema-valid)

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAutoTuner
metadata:
  name: resnet-hpo
  namespace: ml-team
spec:
  searchAlgorithm: bayesian
  objective:
    metricName: val_accuracy
    direction: maximize
  maxTrials: 50
  parallelism: 4
  earlyStoppingRounds: 10
  parameterSpace:
    - name: learning_rate
      type: float
      min: 0.0001
      max: 0.1
      scale: log
    - name: batch_size
      type: int
      values: ["32", "64", "128", "256"]
    - name: optimizer
      type: categorical
      values: ["adam", "sgd", "adamw"]
  jobTemplate:
    type: training
    model: resnet50
    image: training/resnet:latest
    gpus: 1
    gpuType: A100
    resources:
      requests:
        memory: "16Gi"
        cpu: "4"
```

Required spec fields: `searchAlgorithm`, `objective`, `maxTrials`, `parameterSpace`, `jobTemplate`. There is no `budget`, `resumeFrom` or `trialTemplate` field; earlier versions of this guide showed them, and they are rejected by the schema.

### Working with it today

```bash
# Create, list and inspect through kubectl (the gryvia CLI does not manage tuners)
kubectl apply -f resnet-hpo.yaml
kubectl get gryviaautotuners -n ml-team
kubectl get gryviaautotuner resnet-hpo -n ml-team -o yaml
```

`gryvia submit` is for `GryviaAIJob` files, not for these kinds.

### Status fields

The schema defines `status.phase`, `trialsCompleted`, `trialsRunning`, `trialsFailed`, `bestTrial` (name, jobName, parameters, metricValue, ...) and `trials`. The controller fills them.

---

## DAG-Based Pipelines (GryviaWorkflow)

Status: the ai-operator executes the DAG: `job` steps create child `GryviaAIJob`s, `script` steps run a Pod, `webhook` steps are off unless the operator runs with `--workflow-allow-webhooks`. Validated first (at most 100 steps, no cycles); retries, timeouts, skip-on-failure and the `condition` form `steps.<name>.status == 'Succeeded'` are implemented. Unit-tested, the kind e2e (CPU pods) passes in CI, not verified on GPUs.

### Overview

A `GryviaWorkflow` lists `steps`. Each step has a `name`, an optional `dependsOn` list, a `type` (the schema has `jobTemplate` for a job step, `script` for a container script and `webhook` for an HTTP call), `timeoutSeconds`, `retries`, `retryBackoffSeconds` and a free-form `condition` string. Workflow-level `parameters` are a string map. There is no artifact passing, no per-step checkpointing and no structured condition language in the schema. The dependency ordering, fan-out/fan-in, retries and conditions are what a workflow controller would implement; none is running.

### Example (schema-valid)

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaWorkflow
metadata:
  name: image-classifier-pipeline
  namespace: ml-team
spec:
  parameters:
    dataset: "imagenet-1k"
    model_name: "resnet50-v2"
  steps:
    - name: preprocess
      type: job
      timeoutSeconds: 3600
      retries: 2
      jobTemplate:
        type: training
        model: data-pipeline
        image: ml-pipelines/preprocess:1.4.0
        gpus: 0
        command: ["python"]
        args: ["preprocess.py", "--dataset=imagenet-1k", "--output=/data/processed"]
    - name: train
      type: job
      dependsOn: [preprocess]
      timeoutSeconds: 86400
      retries: 1
      jobTemplate:
        type: training
        model: resnet50
        image: training/pytorch-train:2.1.0
        gpus: 4
        gpuType: A100
        network: rdma
        distributed:
          enabled: true
          framework: pytorch
          nodes: 2
          gpusPerNode: 4
          backend: nccl
        command: ["torchrun"]
        args: ["--nproc_per_node=4", "--nnodes=2", "train.py", "--data=/data/processed"]
    - name: evaluate
      type: job
      dependsOn: [train]
      jobTemplate:
        type: evaluation
        model: resnet50
        image: training/pytorch-eval:2.1.0
        gpus: 1
        gpuType: A100
        command: ["python"]
        args: ["evaluate.py"]
```

### Fan-out and fan-in

Several steps that list the same `dependsOn` parent are the intended fan-out, and a step listing several parents is the fan-in:

```text
train -> evaluate ---\
train -> benchmark ---> publish
```

The controller implements the ordering and fan-out shown here. Steps pass small values (paths, scores) to later steps
as outputs: a step writes a JSON object to its termination message (or the `gryvia.io/output-<key>` annotations on its
`GryviaAIJob`) and later steps use `{{steps.<step>.outputs.<key>}}`. A `register` step creates a registry entry and
`spec.schedule` (cron, UTC) reruns the workflow; see [docs/model-factory.md](https://github.com/zyvorai/gryvia/blob/main/docs/model-factory.md#workflow-additions).
Artifacts themselves are not moved: steps share a PVC or object storage.

### Working with it today

```bash
kubectl apply -f pipeline.yaml
kubectl get gryviaworkflows -n ml-team
```

Watch it with `kubectl get gryviaworkflow pipeline -n ml-team -o jsonpath='{.status.stepStatuses}'` (or the dashboard).

---

## Model Registry (GryviaModelRegistry)

Status: the controller mirrors serving state; with `spec.autoServe: true` and `spec.stage: production` it creates a `GryviaInferenceService` named `<entry>-serving`. The gateway lists, gets, registers (`POST /api/models`) and promotes entries. Unit-tested, the kind e2e (tiny CPU images) passes in CI, not verified with a real model server.

### Overview

`GryviaModelRegistry` records a model version: `modelName`, `version`, `artifacts` (`s3Path` or `pvcName`/`subPath`, `format`, `sizeBytes`), `stage`, `source` (`jobRef`, `tunerRef` or `workflowRef`), free-form string `metadata`, and optional `autoServe` with a `servingConfig` (`backend`, `replicas`, `gpuCount`, `gpuType`). The dashboard and gateway treat `stage` as dev, staging and production. Promotion through the gateway (`POST /api/models/{name}/promote`) patches `spec.stage` and validates the transition; with `autoServe` the controller then creates or removes the inference service. `promotionPolicy` promotes a `staging` entry automatically when a metric in `metadata` beats the production version, and `servingConfig.serviceName` rolls each promoted version out as a canary of one shared service ([Model factory](#model-factory-gryviamodelwatch)). There is no human approval workflow.

### Example (schema-valid)

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaModelRegistry
metadata:
  name: resnet50-v2-1-0
  namespace: ml-team
spec:
  modelName: resnet50-v2
  version: "1.0.0"
  source:
    workflowRef: image-classifier-pipeline
  artifacts:
    s3Path: "s3://ml-models/resnet50-v2/v1.0.0/"
    format: pytorch
    sizeBytes: 102400000
  stage: staging
  description: "ResNet50 v2 trained on ImageNet-1K"
  metadata:
    framework: "pytorch-2.1.0"
    dataset: "imagenet-1k"
    top1_accuracy: "0.785"
  autoServe: false
```

### Stage changes

```bash
kubectl get gryviamodelregistries -n ml-team

# What the gateway does on promote: patch spec.stage
kubectl patch gryviamodelregistry resnet50-v2-1-0 -n ml-team \
  --type merge -p '{"spec":{"stage":"production"}}'
```

The controller writes `phase`, `servingEndpoint`, `inferenceServiceName` and the other status fields (see [docs/ml-controllers.md](https://github.com/zyvorai/gryvia/blob/main/docs/ml-controllers.md#gryviamodelregistry)). With shared serving a failing canary is rolled back automatically; otherwise rollback is promoting an earlier entry by hand.

---

## Inference Serving (GryviaInferenceService)

Status: CRD and gateway/dashboard CRUD only. Nothing creates a Deployment, Service or canary. For serving today, run your own Deployment or use a `GryviaAIJob` with `type: inference`.

### Overview

`GryviaInferenceService` declares `modelRef` (a string naming a registry entry), `backend`, `replicas`, `gpuCount`, `gpuType`, `image`, `args`, `servicePort`, `autoscaling` (`minReplicas`, `maxReplicas`, `targetGPUUtilization`, `targetRequestsPerSecond`), `canary` (`enabled`, `weight`, `modelVersion`, `autoPromote`, `promoteAfterSeconds`) and `healthCheck` (`path`, `intervalSeconds`, `failureThreshold`, `autoRollback`).

Backends the design has in mind are Triton, vLLM, TensorRT-LLM and TorchServe; the schema takes `backend` as a free string. Feature lists for each backend (dynamic batching, PagedAttention, quantization and so on) belong to those projects, not to Gryvia, and Gryvia does not configure them beyond passing `args` to the container.

### Example (schema-valid)

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaInferenceService
metadata:
  name: llama-70b-serving
  namespace: ml-serving
spec:
  modelRef: llama-70b-v2-0
  backend: vllm
  replicas: 3
  gpuCount: 4
  gpuType: A100
  image: vllm/vllm-openai:0.3.0
  args:
    - "--tensor-parallel-size=4"
    - "--max-model-len=4096"
    - "--gpu-memory-utilization=0.90"
  servicePort: 8080
  autoscaling:
    enabled: true
    minReplicas: 2
    maxReplicas: 8
    targetGPUUtilization: 75
    targetRequestsPerSecond: 100
  canary:
    enabled: true
    weight: 10
    modelVersion: llama-70b-v2-1
    autoPromote: true
    promoteAfterSeconds: 3600
  healthCheck:
    path: /health
    intervalSeconds: 30
    failureThreshold: 3
    autoRollback: true
```

### Canary behaviour

The controller runs a canary Deployment sized so about `canary.weight` percent of the pods are canary pods (a pod-count split behind one Service, not weighted routing), promotes after `promoteAfterSeconds` if healthy, and deletes the canary after `failureThreshold` failed health checks (the stable Deployment is not rolled back). The step-wise weight schedule and latency or error-rate thresholds that earlier versions of this guide described are not in the schema. No such rollout runs today.

### Working with it today

```bash
kubectl apply -f inference-service.yaml
kubectl get gryviainferenceservices -n ml-serving
```

---

## Interactive Workspaces (GryviaWorkspace)

Status: the ai-operator creates a Pod, a ClusterIP Service and (with `spec.storage`) a PVC per workspace; `spec.paused` (also toggled by the gateway's pause and resume routes) deletes the Pod and keeps the PVC. Unit-tested, the kind e2e passes in CI with a CPU image, not verified with the real Jupyter or code-server images on GPUs. Details: [ML controllers](https://github.com/zyvorai/gryvia/blob/main/docs/ml-controllers.md#gryviaworkspace).

### Overview

`GryviaWorkspace` declares an environment `type` (required, for example `jupyter`), `gpuCount`, `gpuType`, `image`, `storage` (a size string) and `storageClassName`, CPU and memory requests and limits (`cpuRequest`, `cpuLimit`, `memRequest`, `memLimit`), `idleTimeoutMinutes`, `maxLifetimeHours`, `paused` and `env` (a string map). `idleTimeoutMinutes` marks the workspace `Idle` from the `gryvia.io/last-activity` annotation (nothing watches notebook traffic), and `maxLifetimeHours` pauses it; package installation and git cloning are not implemented; the earlier `packages`, `git`, `idleAction` and structured `storage` blocks are not in the schema.

### Example (schema-valid)

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaWorkspace
metadata:
  name: research-notebook
  namespace: ml-team
spec:
  type: jupyter
  gpuCount: 2
  gpuType: A100-80G
  image: ml-images/jupyterlab-cuda:12.1-pytorch2.1
  storage: "100Gi"
  storageClassName: fast-ssd
  cpuRequest: "8"
  cpuLimit: "16"
  memRequest: "32Gi"
  memLimit: "64Gi"
  idleTimeoutMinutes: 60
  maxLifetimeHours: 72
  env:
    WANDB_PROJECT: "ml-research"
    HF_HOME: "/workspace/.cache/huggingface"
```

### Working with it today

```bash
kubectl apply -f workspace.yaml
kubectl get gryviaworkspaces -n ml-team

# Sets spec.paused: the controller deletes the Pod and keeps the PVC and Service
kubectl patch gryviaworkspace research-notebook -n ml-team \
  --type merge -p '{"spec":{"paused":true}}'

kubectl delete gryviaworkspace research-notebook -n ml-team
```

Status fields (`phase`, `url`, `podName`, `lastActivity`) are written by the controller. `url` is cluster-internal: there is no ingress, TLS or authentication in front of the workspace Service.

---

## Model factory (GryviaModelWatch)

Status: opt-in (`aiOperator.modelWatch.enabled`). Unit-tested; the e2e control-plane flow (stand-in hub, busybox steps) passed on a k3s cluster without GPUs and in kind CI. No real download, fine-tune or evaluation has run. Full reference: [docs/model-factory.md](https://github.com/zyvorai/gryvia/blob/main/docs/model-factory.md).

A watch polls the Hugging Face Hub. The first poll records existing models as the baseline; each later model that passes the filters gets a workflow rendered from `workflowTemplate`, with `{{model.id}}`, `{{model.revision}}`, `{{model.slug}}`, `{{model.gpus}}` and similar filled in. A typical template downloads, fine-tunes, evaluates and registers; the registry's `promotionPolicy` and `servingConfig.serviceName` then decide whether the new version replaces the served one.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaModelWatch
metadata:
  name: small-qwen
  namespace: ml-team
spec:
  sources:
    - provider: huggingface
      author: Qwen
      nameRegex: '-Instruct$'
  licenseAllowlist: [apache-2.0]
  maxParamsB: 8
  pollInterval: 6h
  workflowTemplate:
    steps:
      - name: finetune
        type: job
        jobTemplate:
          type: fine-tuning
          image: registry.example.com/gryvia-model-factory:1.0
          gpus: 1
          command: [python3, /app/finetune_lora.py]
          args: ["--base={{model.id}}", "--revision={{model.revision}}", "--name={{model.slug}}"]
      - name: register
        type: register
        dependsOn: [finetune]
        register:
          modelName: chat-assistant
          version: "{{model.slug}}-{{model.revision}}"
          artifacts: {pvcName: models, subPath: "{{steps.finetune.outputs.subPath}}"}
          metadata: {train_loss: "{{steps.finetune.outputs.train_loss}}"}
```

The complete pipeline (download, LoRA fine-tune, lm-eval, register, shared vLLM serving) is [examples/model-factory/model-watch.yaml](https://github.com/zyvorai/gryvia/blob/main/examples/model-factory/model-watch.yaml).

```bash
gryvia models watch create -f watch.yaml -n ml-team
gryvia models watch runs small-qwen -n ml-team
```

---

## Datasets, LLM gateway, RAG and agents

Status: all opt-in. Each piece is unit-tested and runs end to end in the kind workflow (`e2e-ml.yml`) with CPU
stand-ins: a stand-in file server for datasets, small Python servers that return OpenAI-shaped chat, embedding and
tool-call responses, and the real Qdrant image. No real model (vLLM, an embedding model or a tool-calling model) has
been tried, and nothing has run on GPUs.

| Piece | Chart value | What it gives you | Reference |
|-------|-------------|-------------------|-----------|
| Datasets | `storageOperator.datasets.enabled` | `GryviaDataset` downloads an http, s3 or nfs source into a PVC, one directory per version; jobs mount `status.pvcName` at `status.subPath` | [docs/datasets.md](https://github.com/zyvorai/gryvia/blob/main/docs/datasets.md) |
| Evaluation and rollback | none (registry and workflow controllers) | `registry` workflow steps, `rollbackPolicy` on a registry entry, scheduled re-evaluation, AWQ/GPTQ quantize step | [docs/model-evaluation.md](https://github.com/zyvorai/gryvia/blob/main/docs/model-evaluation.md) |
| LLM gateway | `llmGateway.enabled` | One OpenAI-compatible endpoint for every annotated inference service, per-tenant keys, token metering and `tokensPerDay` quotas | [docs/llm-gateway.md](https://github.com/zyvorai/gryvia/blob/main/docs/llm-gateway.md) |
| RAG | `aiOperator.rag.enabled` | `GryviaVectorIndex`: a dataset chunked, embedded through the gateway and kept in Qdrant; `POST /v1/retrieve` on the gateway | [docs/rag.md](https://github.com/zyvorai/gryvia/blob/main/docs/rag.md) |
| Agents | `aiOperator.agents.enabled` | `GryviaAgent`: a tool-calling runtime (retrieval and allowlisted HTTP tools) served as `/v1/chat/completions`, with its own gateway key and an egress NetworkPolicy | [docs/agents.md](https://github.com/zyvorai/gryvia/blob/main/docs/agents.md) |

RAG and agents need the LLM gateway; the chart refuses to render without it. A typical chain:

```bash
helm upgrade gryvia helm/gryvia --reuse-values --set llmGateway.enabled=true \
  --set storageOperator.datasets.enabled=true --set aiOperator.rag.enabled=true --set aiOperator.agents.enabled=true

gryvia datasets create -f examples/datasets/http-dataset.yaml
gryvia rag index create -f examples/rag/vector-index.yaml -n tenant-alpha
gryvia agents create -f examples/agents/agent.yaml -n tenant-alpha
gryvia agents chat helper how are GPU hours capped
```

---

## Practical guidance

These apply to running training today.

- Write checkpoints to a mounted volume from your own training code, and see `GryviaCheckpointGuard` for the operator-side checkpoint handling.
- Set `spec.storage` on a `GryviaAIJob` to get a PVC mounted at `/data`.
- CPU-only work: `gpus: 0` on a `GryviaAIJob` is allowed and runs as a CPU-only Job (no GPU limit, no GPU node selector).
- Add `team` and project labels to jobs so cost and usage reports can group them.

Practices that depend on behaviour the controllers do not have (weighted canary routing, latency-driven autoscaling, activity-based idle detection without something writing `gryvia.io/last-activity`) are not listed here.

---

## Support

- **Issues**: https://github.com/zyvorai/gryvia/issues
- **Discussions**: https://github.com/zyvorai/gryvia/discussions
