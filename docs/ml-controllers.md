# ML controllers

The ai-operator runs five controllers for the ML kinds: `GryviaWorkspace`, `GryviaInferenceService`, `GryviaModelRegistry`,
`GryviaWorkflow` and `GryviaAutoTuner`, plus three opt-in ones: `GryviaModelWatch` (`--enable-model-watch`, see
[Model factory](model-factory.md)), `GryviaVectorIndex` (`--enable-rag`, see [RAG](rag.md)) and `GryviaAgent`
(`--enable-agents`, see [Agents](agents.md)). They are registered in `operators/ai-operator/main.go` (turn them all off with
`--enable-ml-controllers=false`) and the chart's manager ClusterRole carries the permissions they need.

This page says what each one creates, which status fields it writes (the API gateway and the dashboard read exactly
these), the flags that set default images and limits, and what is **not** verified.

## What is verified and what is not

| Verified | How |
| --- | --- |
| Object creation, owner references, idempotent re-reconcile, status fields and their JSON, pause/resume, idle and lifetime handling, canary promotion and rollback, autoServe, DAG order, failure propagation, retries, timeouts, trial launching, parallelism and trial caps, best-trial selection, ASHA and early stopping | Unit tests with a fake client in `operators/ai-operator/controllers/*_test.go` (`go test -race`) |
| The same against a real API server on kind, with tiny CPU images, plus what the gateway returns for each kind | `.github/workflows/e2e-ml.yml` (see "End-to-end test" below). Passes in CI; it also passed on a single-node k3s cluster without GPUs. This covers `GryviaAutoTuner` trials and job-type `GryviaWorkflow` steps running as real batch Jobs |

| Not verified anywhere | Why |
| --- | --- |
| The real Jupyter, code-server, vLLM, Triton, TensorRT-LLM and TorchServe images (the defaults are unpinned or large and were never started) | No GPU, no image pulls of that size in CI |
| GPU scheduling (`nvidia.com/gpu` limits, `gryvia.io/gpu` node selectors, `/dev/shm` for GPU pods) | No GPU |
| An HPA scaling on GPU or requests-per-second metrics | Needs a custom metrics adapter and real load; the e2e scales on CPU only |
| Weighted canary routing | The canary split is by pod count, see below |
| The model factory's real download, fine-tune and evaluation, and the real Hugging Face API | No GPU or model weights in CI; see [Model factory](model-factory.md) |
## GryviaWorkspace

Creates, all named `<workspace>-workspace` and owned by the workspace: a PVC (only when `spec.storage` is set), a bare
Pod and a ClusterIP Service. Deleting the workspace garbage-collects all three, **including the PVC and its data**.

| Spec | Behaviour |
| --- | --- |
| `type` | `jupyter` (port 8888) or `vscode` (port 8080) |
| `image` | Wins over the operator default |
| `gpuCount` | `> 0` adds an `nvidia.com/gpu` limit and a memory-backed `/dev/shm`; `0` or unset is a CPU workspace (no GPU limit) |
| `gpuType` | Node selector `gryvia.io/gpu` (not set for empty or `any`) |
| `storage`, `storageClassName` | The PVC (`ReadWriteOnce`, mounted at `/workspace`) |
| `cpu*`/`mem*` | Requests and limits; an invalid quantity marks the workspace `Failed` with the reason |
| `paused` | Deletes the Pod, keeps the PVC and the Service. Setting it back creates a new Pod that mounts the same PVC. A Pod that is still terminating is waited for |
| `idleTimeoutMinutes` | Sets the phase `Idle` (the Pod keeps running) when there was no activity for that long. Activity is the newest of the session start, the RFC 3339 time in the annotation `gryvia.io/last-activity` and, with the [workspace proxy](workspaces.md) on, the server's own report (Jupyter `/api/status` `last_activity`, code-server `/healthz` `lastHeartbeat`). Without the proxy nothing watches Jupyter or VS Code traffic, so without something that writes the annotation a workspace turns `Idle` that long after it started. Add the annotation `gryvia.io/idle-action: pause` to pause it instead |
| `maxLifetimeHours` | After that long in one session the workspace is paused (`spec.paused` is set, the message says why); resuming starts a new session |

The Pod runs with `allowPrivilegeEscalation: false`, the runtime-default seccomp profile and no service-account
token. The Service is cluster-internal only. Without the [workspace proxy](workspaces.md) there is no ingress, no TLS
and no authentication in front of it, and Jupyter's own token is whatever the image generates; with it, browsers reach
the workspace through the api-gateway and a NetworkPolicy admits only the gateway and the operator. A Pod that ends
(eviction, node loss) is deleted and recreated.

Status written: `phase` (`Pending`, `Running`, `Idle`, `Paused`, `Failed`), `message`, `url` (in-cluster, or the gateway URL with the proxy),
`startTime`, `lastActivity`, `podName`, `pvcName`, `serviceName`, conditions `WorkspaceReady` and `WorkspaceIdle`.

## GryviaInferenceService

Creates `<name>-inference` (a Deployment), a Service of the same name, `<name>-inference-hpa` when autoscaling is
enabled and `<name>-canary` while a canary is enabled. The Service selects stable and canary pods alike.

| Spec | Behaviour |
| --- | --- |
| `backend`, `image` | `spec.image` wins, then the operator default for the backend (below); an unknown backend without an image is `Failed` |
| `servicePort` | Default per backend: 8000 for `vllm`, `triton`, `tensorrt-llm`; 8080 for `torchserve` (the CRD text says 8080) |
| `healthCheck.path` | Probe path: this, else `--inference-health-path`, else per backend (`/health`, `/v2/health/ready`, `/ping`). A startup probe allows up to 30 minutes for the model to load |
| `gpuCount` | `> 0` adds the GPU limit and `/dev/shm`; `0` or unset is a CPU service (no GPU limit) |
| `replicas` | Deployment replicas. With autoscaling on, the HPA owns the count afterwards |
| `modelRef` | The `GryviaModelRegistry` name. Its `artifacts` reach the pods as env `MODEL_S3_PATH`, `MODEL_FORMAT` and, for a PVC, a read-only mount at `/models` (`MODEL_PATH`). The pods also get `MODEL_NAME`, `BACKEND` and, after a promotion, `MODEL_VERSION`. The server image has to use them |
| `autoscaling` | An HPA between `minReplicas` (default 1) and `maxReplicas`. Explicit GPU/RPS targets use custom per-pod metrics and require an adapter; otherwise CPU utilization at 80% is used. Invalid bounds/targets set `AutoscalingValid=False`. `AutoscalingReady` reports the HPA metric status. See [Inference serving](inference-serving.md) |
| `canary` | See below |

Canary: the canary Deployment gets `ceil(stable * w / (100 - w))` replicas so about `weight` percent of the pods are
canary pods. The default is a pod-count split behind one Service. Optional [Gateway API routing](inference-serving.md) provides weighted backend traffic through separate track Services. `status.canaryStatus` tracks
`active`, `weight`, `readyReplicas`, `health` (`Pending`, `Healthy`, `Unhealthy`, `Promoted`, `RolledBack`) and
`startedAt`.

* `autoPromote`: once the canary is healthy and `promoteAfterSeconds` (0 = at once) have passed, the version is
  recorded on the stable Deployment (annotation `gryvia.io/promoted-version`), the stable pods roll out with
  `MODEL_VERSION` set to it and the canary is deleted. A promoted version is never started as a canary again.
* `healthCheck.autoRollback`: a canary with no ready pod, after `--inference-canary-startup-grace`, counts one failure per
  `healthCheck.intervalSeconds` (default 30). `failureThreshold` failures (default 3) delete the canary and record the
  version (`gryvia.io/rolled-back-version`); it is not retried until you choose another version. **Rollback means the
  canary is removed; the stable Deployment is never rolled back.** `consecutiveFailures` and `lastHealthCheck`
  refer to the canary.
* Without `autoPromote` the canary runs until you disable it; there is no manual promote action.

Status written: `phase` (`Deploying`, `Ready`, `Failed`), `readyReplicas`, `endpoint`, `deploymentName`,
`serviceName`, `healthStatus`, `canaryStatus`, `consecutiveFailures`, `lastHealthCheck`, `message`. There is no
latency field: the dashboard's old `latencyMs` was removed because nothing produces it.

## GryviaModelRegistry

A registry entry is metadata. With `spec.autoServe: true` **and** `spec.stage: production` the controller creates a
`GryviaInferenceService` named `<entry>-serving` (owned by the entry, `modelRef` = the entry's name) from
`servingConfig` (`backend` default `vllm`, `replicas` default 1, `gpuCount`, `gpuType`), updates it when
`servingConfig` changes, and mirrors its state. Leaving production or turning `autoServe` off (for example
promoting to `archived`) deletes the service and clears the serving status. An unowned service with that name is
never touched (`Failed`, message).

With `servingConfig.serviceName` all production versions share one service instead, and a newly promoted version is
rolled out as its canary; `spec.promotionPolicy` promotes a `staging` entry to production when its metric beats the
current production version. Both are described in [Model factory](model-factory.md).

`servingConfig.gpuCount` unset or 0 uses `--autoserve-default-gpu-count` (default 1; 0 serves on CPU).

Status written: `phase` (`Registered`, `Deploying`, `Serving`, `Failed`), `servingEndpoint`, `health`,
`inferenceServiceName`, `deployedAt`, `registeredAt`, `previousVersion` (another production version of the same
`modelName`, or the version a shared-service canary replaced), `promotionDecision` (`Promoted`, `Rejected`, `Waiting`),
`message`; phases `Canary` and `RolledBack` for shared serving.

The gateway's `POST /api/models` registers an entry (`name`, `version`, `artifacts` with an `s3Path` and/or `pvcName`,
optional `modelName`, `stage` (`dev`, `staging`, `production`), `sourceJob`, `description`, `autoServe`,
`servingConfig`) in the caller's namespace; `POST /api/models/{name}/promote` walks `dev` to `staging` to `production`
to `archived`; `POST /api/models/{name}/rollback` asks for a rollback of a shared service to `previousVersion`
(`spec.rollbackPolicy` does it automatically when a metric drops; see
[Model factory](model-factory.md#continuous-evaluation-and-rollback)).

## GryviaWorkflow

Steps run as soon as everything in `dependsOn` is finished. The workflow is validated first (at most 100 steps,
DNS-label step names, a payload for the step type, known dependencies, no cycles, supported conditions); an invalid one
goes straight to `Failed` and creates nothing.

| Step type | What runs |
| --- | --- |
| `job` | A child `GryviaAIJob` `<workflow>-<step>` from `jobTemplate`, with the workflow `parameters` and `WORKFLOW_NAME`/`WORKFLOW_STEP` as env. **Needs the AIJob controller to run it** |
| `script` | A Pod `<workflow>-<step>` owned by the workflow: `runAsNonRoot` (uid 65534), no privilege escalation, all capabilities dropped, seccomp runtime default, no service-account token, CPU/memory requests and limits (100m/128Mi and 1/1Gi), `activeDeadlineSeconds` from `timeoutSeconds`. Does not need the AIJob controller |
| `register` | Creates a `GryviaModelRegistry` entry from the step's `register` fields; see [Model factory](model-factory.md) |
| `registry` | Merges metadata into an existing entry, or requests its rollback (`entry`, or `serviceName` for the entry a shared service serves); see [Model factory](model-factory.md) |
| `webhook` | One HTTP call from the operator. **Off unless `--workflow-allow-webhooks`** (a webhook step lets whoever can create a workflow make the operator send requests inside the cluster network); off means the step fails with a message. Success is a 2xx answer or `status == <code>` in `successCondition` |

* A step whose dependency failed or was skipped is skipped (the skip cascades), unless it has a `condition`.
  Conditions are `steps.<name>.status == 'Succeeded'` (or `!=`, phases `Succeeded`, `Failed`, `Skipped`, `Running`,
  `Pending`) joined with `&&`, on steps listed in `dependsOn`; that is how an "on failure" step is written.
* `retries` (max 10) start a new child per attempt (`<name>-r1`, ...), after `retryBackoffSeconds`; the failed
  attempt's object is kept. `timeoutSeconds` fails (and deletes) a running step.
* At most `--workflow-max-parallel-steps` (default 10) steps of one workflow run at a time. A failed step does not
  cancel steps that already run.
* The workflow is `Failed` when any step failed and nothing is left to run, else `Succeeded`.
* Steps report outputs (termination message JSON or `gryvia.io/output-<key>` annotations) that later steps use as
  `{{steps.<step>.outputs.<key>}}`; `{{parameters.<name>}}`, `{{workflow.name}}` and `{{workflow.run}}` are filled too.
  `spec.schedule` (5-field cron, UTC) reruns the workflow (phase `Scheduled` in between). Details in
  [Model factory](model-factory.md#workflow-additions).

Status written: `phase` (`Pending`, `Running`, `Succeeded`, `Failed`), `startTime`, `completionTime`, `message`,
`stepStatuses[]` with `name`, `phase` (`Pending`, `Running`, `Succeeded`, `Failed`, `Skipped`), `jobName` (the child
object, a Pod for script steps), `startTime`, `completionTime`, `retriesAttempted`, `outputs`, `message`; with a
schedule also `run`, `lastScheduleTime`, `nextScheduleTime`. A step waiting for a
retry is `Pending` and keeps the failed attempt's `completionTime`.

## GryviaAutoTuner

Every trial is a child `GryviaAIJob` `<tuner>-trial-<n>` created from `jobTemplate` with the trial's hyperparameters as
`HP_<name>` env plus `TRIAL_NAME` and `TUNER_NAME`. **Needs the AIJob controller to run it.**

* At most `min(spec.parallelism, --tuner-max-parallelism)` trials run at once (default 1 and 32), and `maxTrials` is
  capped by `--tuner-max-trials` (default 1000): a tuner asking for more is `Failed`. A grid over more than 100000
  combinations, an empty parameter space or a template without an image are `Failed` too.
* Failed trials count towards `maxTrials`. A template the API server rejects becomes a failed trial, so it cannot loop.
  A grid that runs out of points ends the tuner early ("Search space exhausted"). No successful trial at all makes the
  tuner `Failed`.
* **The objective metric of a trial** is read when its job succeeds from the annotation
  `gryvia.io/metric-<metricName>` on the `GryviaAIJob` (characters other than `A-Za-z0-9_.-` become `_`), or, for an
  objective named `loss`, from `status.metrics.loss`. Nothing writes that annotation by itself: whatever runs the
  trial has to. A tuner whose trials report nothing succeeds without a best trial and says so.
* `earlyStoppingRounds`: that many finished trials after the best one without an improvement end the tuner and stop the
  running trials. `searchAlgorithm: asha` with `ashaConfig` deletes running trials that fall behind at their rung, using
  `status.metrics.epoch`/`loss` of the job as intermediate results.

Status written: `phase` (`Pending`, `Running`, `Succeeded`, `Failed`), `trialsRunning`, `trialsCompleted` (succeeded
plus stopped), `trialsFailed`, `bestTrial`, `trials[]` (`name`, `parameters`, `metricValue`, `phase`, `jobName`,
`startTime`, `completionTime`, `message`, `intermediateMetrics`), `startTime`, `completionTime`, `message`. The phase word
for work in progress is `Running` because that is what the dashboard and the gateway know.

## Flags of the ai-operator

Pass them with the chart value `aiOperator.extraArgs` (a list of strings, empty by default).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--enable-ml-controllers` | `true` | Run the five controllers |
| `--workspace-jupyter-image` | `quay.io/jupyter/pytorch-notebook:cuda12-latest` | Image of jupyter workspaces without `spec.image` |
| `--workspace-code-image` | `codercom/code-server:latest` | Image of vscode workspaces without `spec.image` |
| `--inference-image-vllm` | `vllm/vllm-openai:latest` | Per-backend image without `spec.image` |
| `--inference-image-triton` | `nvcr.io/nvidia/tritonserver:24.01-py3` | |
| `--inference-image-tensorrt-llm` | `nvcr.io/nvidia/tritonserver:24.01-trtllm-python-py3` | |
| `--inference-image-torchserve` | `pytorch/torchserve:latest-gpu` | |
| `--inference-health-path` | empty (per backend) | Probe path for every backend |
| `--inference-canary-startup-grace` | `5m` | Time a new canary may take to become ready |
| `--autoserve-default-gpu-count` | `1` | GPUs of an auto-served model without `servingConfig.gpuCount` |
| `--workflow-max-parallel-steps` | `10` | |
| `--workflow-allow-webhooks` | `false` | |
| `--tuner-max-trials` | `1000` | |
| `--tuner-max-parallelism` | `32` | |
| `--enable-model-watch` | `false` | Run the `GryviaModelWatch` controller (chart: `aiOperator.modelWatch.enabled`) |
| `--model-watch-hub-url` | `https://huggingface.co` | Hub every watch polls (chart: `aiOperator.modelWatch.hubURL`) |
| `--model-watch-min-poll-interval` | `5m` | Shortest `pollInterval` honoured (chart: `aiOperator.modelWatch.minPollInterval`) |
| `--enable-rag` | `false` | Run the `GryviaVectorIndex` controller (chart: `aiOperator.rag.enabled`) |
| `--rag-qdrant-image` | `qdrant/qdrant:v1.12.6-unprivileged` | Managed store image (chart: `aiOperator.rag.qdrantImage`) |
| `--rag-ingest-image` | `ghcr.io/zyvorai/gryvia-rag-ingest:latest` | Ingestion Job image (chart: `aiOperator.rag.ingestImage`) |
| `--enable-agents` | `false` | Run the `GryviaAgent` controller (chart: `aiOperator.agents.enabled`) |
| `--agent-image` | `ghcr.io/zyvorai/gryvia-agent-runtime:latest` | Agent runtime image (chart: `aiOperator.agents.image`) |
| `--llm-gateway-url` | | LLM gateway the ingestion Jobs and agents call; required with `--enable-rag` or `--enable-agents` (the chart derives it) |
| `--llm-key-namespace` | `gryvia-llm-keys` | Where index and agent keys are hashed (chart: `llmGateway.keyNamespace`) |

Example for a cluster without GPUs (what the e2e uses):
`aiOperator.extraArgs={--workspace-code-image=nginxinc/nginx-unprivileged:1.27-alpine,--inference-image-torchserve=nginxinc/nginx-unprivileged:1.27-alpine,--inference-health-path=/,--autoserve-default-gpu-count=0}`.

## RBAC

The chart's manager ClusterRole gains: the five kinds with `/status` and `/finalizers`; `apps/deployments` and
`autoscaling/horizontalpodautoscalers` (create, get, list, watch, patch, delete); and `patch` on Services. Pods, Services and PVCs
were already covered. The gateway's ClusterRole already had read on the five kinds and create/delete/patch/update on
them (pause and resume are a patch of `spec.paused`, promote a patch of `spec.stage`, `POST /api/models` a create).
Job steps and tuner trials use the existing `gryviaaijobs` permissions. `GryviaTemplate` and `GryviaPriority`
controllers stay unregistered: templates only matter once something instantiates them, and the priority controller
evaluates preemption of jobs, which is done by the opt-in Kueue integration instead (docs/kueue-integration.md).
For the model factory it also has `gryviamodelwatches` (with `/status` and `/finalizers`), and the gateway and tenant
roles carry the same verbs on it as on `gryviaautotuners`.

## End-to-end test

`.github/workflows/e2e-ml.yml` (helpers in `scripts/e2e-ml-lib.sh`) builds the images, installs the chart on kind, restarts
the ai-operator with the tiny-image flags above and checks, on the cluster and through the gateway:

* Workspace: Pod, Service and PVC exist, phase `Running`, `url`, `startTime`, `lastActivity`; pause through the gateway
  removes the Pod and keeps the PVC (same UID), phase `Paused`; resume brings it back on the same PVC; a workspace created
  and deleted through the gateway is cleaned up.
* ModelRegistry: `POST /api/models`, promote, `autoServe` creates `<name>-serving`, `servingEndpoint` is set, matches the
  gateway and answers from inside the cluster; archiving removes the service.
* InferenceService: Deployment, Service, `readyReplicas`, endpoint answers, HPA with the requested range, canary
  Deployment and `canaryStatus`, auto-promotion (annotation, `MODEL_VERSION` on the stable pods, canary removed and not
  recreated); a service created and deleted through the gateway; with metrics-server installed, the default CPU HPA
  reads real metrics (`ScalingActive`, mirrored to `AutoscalingReady`) and scales a CPU-bound stand-in to
  `maxReplicas`, and the controller leaves that count alone. Canary rollback is covered by unit tests only.
* Workflow: two script steps run in order (timestamps compared) and succeed; a failing step skips its dependent; the gateway
  shows the phases, steps and order. A two-step DAG of CPU **jobs** runs through the AIJob controller's batch Jobs.
* AutoTuner: 3 trials with parallelism 2, best trial and gateway fields (trials are batch-Job-backed `GryviaAIJob`s).
* Model factory: a stand-in hub, baseline and license/size filters, step outputs from a script pod and from a
  `GryviaAIJob`, a register step, auto-promotion onto a shared service, a better version canaried and promoted (the
  replaced one archived), a worse one rejected, suspend through the gateway. The steps are busybox stand-ins.

Opt-in canary SLO evaluation uses the administrator-configured `--inference-prometheus-url` (Helm
`aiOperator.inferencePrometheusURL`) and per-service error-rate/latency threshold annotations. It requires
normalized request/error counters and a duration histogram. Missing telemetry blocks promotion; measured
breaches follow `healthCheck` rollback policy. See [Inference serving](inference-serving.md).
