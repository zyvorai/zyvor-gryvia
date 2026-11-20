# GPU-cluster runbook: verifying the AI features on real hardware

Every AI feature in Gryvia has passed its control-plane e2e (`.github/workflows/e2e-ml.yml`, kind and a single-node k3s
host, CPU only), but the parts that need a GPU have never run: real images, a real fine-tune, AWQ quantization, vLLM
serving the result as a canary, and vLLM behind the LLM gateway, RAG and agents. This runbook walks one small open model
through all of them on a cluster with GPUs and says, for each phase, which "Not verified" row of which doc the result
settles. **Nothing in this runbook has been run yet**; the first run is the verification, and anything that does not
behave as written is a finding to report.

Node bring-up (driver, device plugin, `GryviaGpuNode` registration, a first GPU job) is
[GPU validation](gpu-validation.md); do that first. This runbook starts where it ends.

## What you need

| Item | Why |
| --- | --- |
| **Two NVIDIA GPUs** with compute capability 8.0 or newer and at least 24 GB each (L4, A10/A10G, L40S, A100, H100), on one or two nodes | The canary phase runs the stable and the canary version at the same time, one GPU each; with one GPU the canary stays Pending and is rolled back after `--inference-canary-startup-grace`. bf16 and the 4-bit Marlin kernels vLLM uses for AWQ weights need 8.0+, so no T4 or V100 |
| A ReadWriteMany storage class | The `models` PVC is written by the workflow jobs and mounted read-only by every serving pod (NFS, CephFS, EFS, Filestore, Longhorn RWX) |
| A container registry you can push to | For the model factory image |
| A Hugging Face account with a write token | To publish the model the watch will discover (below) |
| Outbound HTTPS from the cluster to `huggingface.co`, `pypi.org` (image build only) and Docker Hub | Model downloads, image pulls |
| Gryvia from `main`, the `gryvia` CLI, `kubectl`, `helm`, `jq`, `huggingface-cli` (`pip install -U huggingface_hub`) | |

Roughly 2 to 3 hours of GPU time, most of it image pulls and the fine-tune. The model is
[Qwen2.5-0.5B-Instruct](https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct) (Apache-2.0, about 1 GB), small enough that
every step is quick; the agent phase uses Qwen2.5-3B-Instruct because tool calling with 0.5B is unreliable.

**Why the model is republished under your account.** The watch lists an author's models by last modification, at most
100 per poll (`pkg/modelhub/huggingface.go`), so an older model from a large organization such as Qwen may never be
listed. Publishing a copy under your own account after the watch has recorded its baseline also tests what the model
factory is for: a new release appears and a run starts.

## Record as you go

Make a directory for the evidence and keep it with the report:

```bash
export EVID=$PWD/gpu-ai-evidence-$(date -u +%Y%m%d); mkdir -p "$EVID"
export NS=ml-team
kubectl version -o yaml > "$EVID/kubectl-version.yaml"
kubectl get nodes -o wide > "$EVID/nodes.txt"
kubectl get nodes -o json | jq '[.items[] | {name: .metadata.name, gpu: .status.allocatable["nvidia.com/gpu"], product: .metadata.labels["nvidia.com/gpu.product"], gryvia: .metadata.labels["gryvia.io/gpu"]}]' > "$EVID/gpus.json"
helm -n gryvia-system get values gryvia > "$EVID/values-before.yaml"
git rev-parse HEAD > "$EVID/gryvia-commit.txt"
```

At the end of each phase, save what it lists under **Evidence**. Pin every image tag you use (vLLM in particular: its
flags change between releases) and write the tags into the report.

## Phase 0: install with the AI features on

```bash
helm upgrade gryvia helm/gryvia -n gryvia-system --reuse-values \
  --set aiOperator.modelWatch.enabled=true \
  --set llmGateway.enabled=true \
  --set aiOperator.rag.enabled=true \
  --set aiOperator.agents.enabled=true \
  --set storageOperator.enabled=true --set storageOperator.datasets.enabled=true \
  --set-json 'aiOperator.extraArgs=["--inference-image-vllm=vllm/vllm-openai:<tag>"]' \
  --wait --timeout 10m
kubectl create namespace "$NS"
```

`--reuse-values` keeps your existing settings; if `aiOperator.extraArgs` was already set, add the vLLM flag to the
existing list instead. Expected: every pod in `gryvia-system` Running, including `gryvia-llm-gateway`.

**Evidence:** `kubectl -n gryvia-system get pods -o wide`, `helm -n gryvia-system get values gryvia`.

## Phase 1: the default ML images on a GPU

Settles in [ML controllers](ml-controllers.md): "the real Jupyter, code-server, vLLM … images" and "GPU scheduling".

```bash
kubectl apply -n "$NS" -f - <<'EOF'
apiVersion: gryvia.io/v1alpha1
kind: GryviaWorkspace
metadata: {name: gpu-notebook}
spec: {type: jupyter, gpuCount: 1, gpuType: any, storage: 20Gi, idleTimeoutMinutes: 30}
---
apiVersion: gryvia.io/v1alpha1
kind: GryviaInferenceService
metadata: {name: stock-qwen}
spec:
  modelRef: stock-qwen          # no registry entry: vLLM downloads the model itself
  backend: vllm
  replicas: 1
  gpuCount: 1
  gpuType: any
  args: ["--model=Qwen/Qwen2.5-0.5B-Instruct", "--max-model-len=4096"]
EOF
kubectl -n "$NS" get gryviaworkspace,gryviainferenceservice -w        # both Ready (the images are large: minutes)
kubectl -n "$NS" exec $(kubectl -n "$NS" get pod -l gryvia.io/workspace=gpu-notebook,gryvia.io/component=workspace-pod -o name) -- nvidia-smi -L
kubectl -n "$NS" port-forward svc/stock-qwen-inference 8000:8000 &
curl -s localhost:8000/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen2.5-0.5B-Instruct","messages":[{"role":"user","content":"Say hello"}],"max_tokens":20}' | jq .
```

Expected: both reach Ready on GPU nodes; the workspace pod sees one GPU; the pods have the `nvidia.com/gpu: 1` limit and
a memory-backed `/dev/shm`; vLLM answers. Delete both afterwards to free the GPUs:
`kubectl -n "$NS" delete gryviaworkspace gpu-notebook; kubectl -n "$NS" delete gryviainferenceservice stock-qwen`.

**Evidence:** both objects' YAML, `kubectl -n "$NS" get pods -o yaml` (limits, volumes, node), the `nvidia-smi -L`
output, the vLLM log head (`kubectl logs` of the inference pod: version, GPU, KV cache size) and the chat response.

## Phase 2: the model factory: download, fine-tune, quantize, evaluate, register, serve

Settles in [Model factory](model-factory.md): "a real download, fine-tune, quantization or evaluation", "vLLM serving
the quantized (compressed-tensors) weights", "the real Hugging Face API", "vLLM loading a fine-tuned model from the
registry PVC"; in [Model evaluation](model-evaluation.md): "a real evaluation" and "a real AWQ or GPTQ quantization".

**Image and data.**

```bash
docker build -t <registry>/gryvia-model-factory:runbook examples/model-factory
docker push <registry>/gryvia-model-factory:runbook
```

Create the PVCs `models` (ReadWriteMany, 50Gi) and `datasets` (10Gi) in `$NS`, and put two files into `datasets`
through a temporary pod: `chat.jsonl` with a few hundred `{"messages": [...]}` records (any instruction data whose
licence allows it), and `eval.jsonl` with 50 to 100 `{"prompt": "...", "expected": "..."}` records whose answers are a
short exact string (for example arithmetic: `{"prompt": "Q: What is 17+25?\nA:", "expected": "42"}`). Record the
sources and line counts.

**The watch.** Copy `examples/model-factory/model-watch.yaml` and change:

| Field | Value | Why |
| --- | --- | --- |
| Secret `hf-token` | your read token | Your account's models are public, but keep the token path exercised |
| `spec.sources` | one source: `{provider: huggingface, author: <your-hf-user>, nameRegex: '^gryvia-runbook-', limit: 20}` | Only the model you publish |
| `licenseAllowlist` | `[apache-2.0]` | |
| `maxParamsB` | `1` | |
| `pollInterval` | `5m` | The chart's `minPollInterval` floor |
| `sizing.gpuMemoryGB` | your GPU's memory (for example `24`) | The estimate for 0.5B is then 1 GPU |
| every `image` | `<registry>/gryvia-model-factory:runbook` | |
| every `gpuType`, and `servingConfig.gpuType` | `any`, or your nodes' `gryvia.io/gpu` value | The example says `H100` |
| `finetune` step args | add `"--max-steps=60"` | A short run; the point is that it runs, not the quality |
| `evaluate` step args | `--tasks=arc_easy` and `--limit=200` | Faster; `--custom` stays |
| `servingConfig.promoteAfterSeconds` | `300` | |

Apply it, and only when the watch has polled once (status `Watching`, `lastPollTime` set) publish the model:

```bash
kubectl apply -f model-watch-runbook.yaml
kubectl -n "$NS" get gryviamodelwatch open-llms -o jsonpath='{.status.phase} {.status.lastPollTime}{"\n"}'
huggingface-cli download Qwen/Qwen2.5-0.5B-Instruct --local-dir ./qwen-0.5b
huggingface-cli upload <your-hf-user>/gryvia-runbook-qwen2.5-0.5b-instruct ./qwen-0.5b .
```

The copied model card keeps `license: apache-2.0`, which the licence filter reads. Within one poll interval the watch
lists the model as a candidate and starts a workflow:

```bash
gryvia models watch runs open-llms -n "$NS"
RUN=$(kubectl -n "$NS" get gryviaworkflow -l gryvia.io/model-watch=open-llms -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$NS" get gryviaworkflow "$RUN" -w
kubectl -n "$NS" get gryviaworkflow "$RUN" -o jsonpath='{.status.stepStatuses}' | jq .
```

Expected, step by step:

| Step | Expected | Look at |
| --- | --- | --- |
| download | Succeeded; outputs `path`, `subPath` under `cache/` | the step pod's log |
| finetune | Succeeded on a GPU; outputs `train_loss` (a number) and `subPath` under `finetuned/` | `nvidia-smi` during the run, log |
| quantize | Succeeded; outputs `format: compressed-tensors`, `method: awq`, `scheme: W4A16_ASYM`; the output directory is much smaller than the fine-tuned one | `du -sh` in a pod mounting `models` |
| evaluate | Succeeded; output `score` between 0 and 1 | log: per-task numbers |
| register | a `GryviaModelRegistry` entry `chat-assistant` version `<slug>-<revision>`, `eval_score` = the score, promoted to `production` (there is no other production entry) | `kubectl -n "$NS" get gryviamodelregistries -o yaml` |
| serving | `GryviaInferenceService` `chat-assistant` created, vLLM loads the quantized weights from the PVC (`--quantization=compressed-tensors`), Ready | the pod log: "compressed-tensors", the kernel it picked |

```bash
kubectl -n "$NS" port-forward svc/chat-assistant-inference 8080:8080 &
curl -s localhost:8080/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"chat-assistant","messages":[{"role":"user","content":"What is 17+25?"}],"max_tokens":10}' | jq .
```

**Evidence:** the watch YAML (status and candidates), the workflow YAML, every step pod's log, the registry entry, the
inference service YAML, the vLLM log head and the response. Record wall-clock time per step and the GPU model.

## Phase 3: a better version rolls out as a canary and is promoted

Settles in [ML controllers](ml-controllers.md) and [Model factory](model-factory.md): the canary path with real vLLM
pods (until now only nginx stand-ins). Needs the second GPU.

Turn on retraining and publish a new revision of the same model:

```bash
kubectl -n "$NS" patch gryviamodelwatch open-llms --type merge -p '{"spec":{"retrainOnNewRevision":true}}'
echo "runbook revision 2" > ./qwen-0.5b/RUNBOOK.md
huggingface-cli upload <your-hf-user>/gryvia-runbook-qwen2.5-0.5b-instruct ./qwen-0.5b .
```

A second workflow runs. Its version is promoted only if its `eval_score` beats the production one by at least
`minDelta` (0.01); with the same data and step count it may not. Both outcomes are results; record which one happened.
If it was rejected and you still need the canary path exercised, lower the production entry's recorded score by hand
and say so in the report. A rejected entry stays in `staging` and is judged again on its next reconcile; the annotation
only makes that happen now:

```bash
kubectl -n "$NS" patch gryviamodelregistry <production-entry> --type merge -p '{"spec":{"metadata":{"eval_score":"0"}}}'
kubectl -n "$NS" annotate gryviamodelregistry <new-entry> runbook.gryvia.io/rejudge="$(date +%s)" --overwrite
```

Expected: the new entry is promoted to `production`, patched in as the canary of `chat-assistant` (`canaryWeight` 10),
a second vLLM Deployment starts on the other GPU and becomes Ready, and after `promoteAfterSeconds` (300) the service's
`modelRef` becomes the new entry, the old entry is `archived` and recorded as `status.previousVersion`, and the old
Deployment goes away. Send a few requests during the canary window; both versions should answer.

**Evidence:** `kubectl -n "$NS" get gryviainferenceservice chat-assistant -o yaml` before, during and after,
`kubectl -n "$NS" get deploy,pods -o wide` during the canary, both entries' YAML, and the operator log lines for the
service (`kubectl -n gryvia-system logs deploy/gryvia-ai-operator | grep chat-assistant`).

## Phase 4: a scheduled evaluation rolls the served version back

Settles in [Model evaluation](model-evaluation.md): "a real evaluation … against a live endpoint".

Copy `examples/model-factory/eval-schedule.yaml`, set the image and `schedule: "*/10 * * * *"`, and apply it. The next
run scores the served version through vLLM and writes `live_score` onto its entry. The entries carry
`rollbackPolicy {metric: live_score, threshold: "0.5"}`: below 0.5 the previous version comes back.

Expected: the evaluate step reaches the endpoint (`--endpoint=http://chat-assistant-inference.$NS.svc.cluster.local:8080`)
and reports a score; the record step writes `live_score`. If the score is below 0.5, the served entry is archived, the
previous version is the service's `modelRef` again (condition and Event `RolledBack`) and vLLM serves it. If it is
above 0.5, record it, then force the path by raising the threshold on the served entry
(`spec.rollbackPolicy.threshold` to a value above the score) and wait for the next run. Delete the schedule afterwards.

**Evidence:** the workflow's run statuses, both entries' YAML, the service YAML and Events after the rollback.

## Phase 5: vLLM behind the LLM gateway

Settles in [LLM gateway](llm-gateway.md): "vLLM itself behind the gateway".

```bash
kubectl -n "$NS" annotate gryviainferenceservice chat-assistant gryvia.io/llm-model=chat gryvia.io/llm-served-model=chat-assistant
KEY=$(gryvia llm keys create runbook -n "$NS" -o json | jq -r .key)   # shown only once
kubectl -n gryvia-system port-forward svc/gryvia-llm-gateway 18080:8080 &
curl -s localhost:18080/v1/models -H "Authorization: Bearer $KEY" | jq .
curl -s localhost:18080/v1/chat/completions -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"chat","messages":[{"role":"user","content":"Name three colours"}],"max_tokens":30}' | jq .
curl -sN localhost:18080/v1/chat/completions -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"chat","stream":true,"messages":[{"role":"user","content":"Count to five"}],"max_tokens":30}'
gryvia llm usage --group-by day --days 1
```

Expected: `chat` is listed; the plain and
streamed responses come from vLLM; within the metering interval a `GryviaUsageRecord` of kind tokens appears for
`$NS` with the token counts vLLM reported (`usage` in the response). Then set a `GryviaQuota` with `tokensPerDay: 50`
for `$NS` and confirm the next request gets HTTP 429.

**Evidence:** the responses (including the SSE stream), the usage records' YAML, the 429 response.

## Phase 6: RAG with a real embedding model

Settles in [RAG](rag.md): "a real embedding model served by vLLM".

Free a GPU first (delete the eval schedule; keep `chat-assistant`). Apply `examples/rag/vector-index.yaml` with these
changes: the dataset's `namespace` and the index's namespace `$NS`; the dataset URL
`https://raw.githubusercontent.com/zyvorai/gryvia/main/README.md`; the embedding service in `$NS`, `gpuType: any`, and its `args` with
`--model=BAAI/bge-small-en-v1.5` plus the embedding switch your vLLM tag needs (`--task=embed` in releases that have
it; check `vllm serve --help` for the image you pinned); drop the `handbook-cloud` index.

```bash
gryvia rag index list -n "$NS"                   # handbook: Ready, with chunk and vector counts
curl -s localhost:18080/v1/retrieve -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"index":"handbook","query":"How do I try Gryvia without GPUs?","topK":3}' | jq .
```

Expected: the ingestion Job embeds every chunk through the gateway (the embed service's log shows the requests), the
index is Ready with the counts in its status, and the top result is the README section about trying Gryvia without
GPUs. Record the vector dimension in the index status (384 for bge-small).

**Evidence:** the dataset and index YAML, the ingestion Job log, the embed pod's log head, the retrieve response.

## Phase 7: an agent on a real tool-calling model

Settles in [Agents](agents.md): "a real tool-calling model served by vLLM".

Replace `chat-assistant` with a model that calls tools reliably: delete it (or scale it to zero) and create a service
`tools-chat` with `--model=Qwen/Qwen2.5-3B-Instruct --enable-auto-tool-choice --tool-call-parser=hermes`, annotated
`gryvia.io/llm-model: chat`. Apply `examples/agents/agent.yaml` with namespace `$NS` and the `platform_status` tool's
URL replaced by one the cluster can reach, or the tool removed.

```bash
gryvia agents list -n "$NS"
export GRYVIA_GATEWAY_URL=<api-gateway URL> GRYVIA_API_KEY=<admin API key>   # chat goes through the api-gateway
gryvia agents chat helper how do I try gryvia without gpus
```

Expected: the agent calls `search_handbook` (the runtime's log shows the tool call and the retrieve request), answers
from the retrieved chunks and names the source, within `maxSteps`. Ask a question the handbook does not cover and check
that it says so.

**Evidence:** the agent YAML, the runtime log for both questions, the answers.

## Report and doc updates

Write a short report with: the GPU model and count, driver and CUDA versions, Kubernetes distribution and version, the
Gryvia commit, every image tag, per phase the result (pass, fail or not run) and what differed from this runbook, and
the wall-clock times. Attach `$EVID`.

Then update the docs from the evidence, never ahead of it: move each settled row from the "Not verified" table to the
"Verified" table of the doc named in the phase, with the date and the hardware (for example "passed on 2× L4, k3s
v1.31, vLLM v0.x.y, 2026-mm-dd"), and the same in `CHANGELOG.md`. A phase that failed keeps its row and gains the
finding. Quality numbers (scores, the AWQ loss) describe this 0.5B run only; do not quote them as properties of the
feature.

## Clean up

```bash
gryvia llm keys delete runbook -n "$NS" --yes
kubectl delete namespace "$NS"
helm upgrade gryvia helm/gryvia -n gryvia-system -f "$EVID/values-before.yaml" --wait --timeout 10m
huggingface-cli repo delete <your-hf-user>/gryvia-runbook-qwen2.5-0.5b-instruct   # or delete it on the website
```

The registry entries and the PVC data go with the namespace. Deleting the published model is up to you; while it
exists, any watch with the same filters would pick it up.

## Not covered here

- HPA scaling on real metrics (needs metrics-server or the Prometheus adapter in `examples/inference`).
- Weighted canary routing through Gateway API (`aiOperator.inferenceGatewayRouting`), and Prometheus SLO gating.
- Multi-node training, NCCL, RDMA and the eBPF GPU probes: see [GPU validation](gpu-validation.md),
  [NCCL/RDMA correlation](nccl-rdma-gpu-correlation.md) and [Fabric scheduling](fabric-scheduling.md).
- Triton, TensorRT-LLM and TorchServe serving, and full (non-LoRA) fine-tuning of large models.
