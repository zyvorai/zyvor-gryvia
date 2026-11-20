# Model factory: fine-tune, evaluate and serve new open models automatically

When a new open model is published on the Hugging Face Hub, the model factory fine-tunes it on your data, evaluates it,
registers the result and, if it scores better than the version being served, rolls it out as a canary of the shared
inference service. It is four pieces of the ai-operator working together:

1. **`GryviaModelWatch`** polls the hub, filters models (author, name regex, pipeline tag, downloads, license, size)
   and starts one `GryviaWorkflow` per new model, rendered from the watch's `workflowTemplate`.
2. **`GryviaWorkflow`** runs the steps. Steps report **outputs** that later steps use (`{{steps.finetune.outputs.path}}`),
   and a **`register` step** creates the `GryviaModelRegistry` entry.
3. **`GryviaModelRegistry.spec.promotionPolicy`** moves the entry to `production` only if its metric beats the
   production version of the same `modelName`.
4. **`servingConfig.serviceName`** makes all versions share one `GryviaInferenceService`: a newly promoted version is
   started as its canary, promoted after `promoteAfterSeconds` of health (the replaced version is archived) or rolled
   back.
5. **`rollbackPolicy`** and a scheduled evaluation keep checking the served version and bring the previous one back
   when its score drops (see [Continuous evaluation and rollback](#continuous-evaluation-and-rollback)).

A ready-made watch, image and scripts (download, TRL/PEFT LoRA fine-tune, llm-compressor AWQ/GPTQ quantization,
lm-evaluation-harness) are in
[examples/model-factory](../examples/model-factory/README.md).

## What is verified and what is not

| Verified | How |
| --- | --- |
| Hub client (query parameters, token, response size cap, parsing, gated models, size estimates from the name) | Unit tests against an `httptest` server (`operators/ai-operator/pkg/modelhub`) |
| Watch: baseline, filters, dedupe, retrain on new revision, GPU sizing, concurrency, suspend, failures; workflow outputs, templating, register step, cron schedule; promotion policy and the shared-service canary rollout through both controllers | Fake-client unit tests in `operators/ai-operator/controllers/*_test.go` (`go test -race`) |
| The control-plane flow on a real API server: a stand-in hub, baseline and filters, outputs from a script step and from a `GryviaAIJob` termination message, auto-promotion, the shared service, a better version canaried and promoted, a worse one rejected, suspend through the gateway | `.github/workflows/e2e-ml.yml` ("Model factory" steps) with busybox steps that only emit outputs. These three steps passed on a single-node k3s cluster (no GPU) and in the kind workflow in CI on 2026-10-01 |
| Serving a fine-tuned model from the registry PVC with a real engine, and the scheduled-evaluation pattern: `convert_gguf.py` writes a q8_0 GGUF next to the merged weights, the registered entry points at that directory, a `GryviaInferenceService` with `modelRef` to the entry runs llama.cpp's server on the GGUF mounted from the PVC, and a second workflow scores the served model through its endpoint and writes `live_score` onto the entry with a registry step. That step uses `evaluate.py --endpoint` on the facts and lm-eval `gsm8k` (8 questions) through the same endpoint, with `--tokenizer` pointing at the merged model next to the GGUF | The "Model factory - real download" step of `.github/workflows/e2e-ml.yml`. Locally the served q8_0 GGUF scored 1.0 on the facts and 0 of 8 on `gsm8k` (a 135M model) through llama-server's `/v1/completions` (llama.cpp b11146; the converter from b11347) |
| The watch against the real Hugging Face API (`GET /api/models`, no token): a first poll records SmolLM2 base models as the baseline with the hub's revision, license and size (0.13B for SmolLM2-135M from the safetensors metadata); with `includeExisting`, SmolLM2-1.7B (1.71B) is rejected by `maxParamsB: 1` and SmolLM2-135M's workflow receives the revision the hub reports | The "Model watch - the real Hugging Face API" step of `.github/workflows/e2e-ml.yml` |
| The Python scripts' argument handling, output files, data validation and score aggregation | `examples/model-factory/test_model_factory.py` (no GPU, no network; CI job "Model factory scripts") |
| A real download, LoRA fine-tune and evaluation on CPU, as `GryviaWorkflow` job steps: `download.py` fetches SmolLM2-135M from huggingface.co at a pinned revision onto a PVC, `evaluate.py` scores the base model on eight made-up facts, `finetune_lora.py --cpu` trains a LoRA on them and merges it, `evaluate.py` scores the merged model, and the register step records both scores, the train loss and the PVC path; the first version is promoted | The "Model factory - real download" step of `.github/workflows/e2e-ml.yml` with the image from `examples/model-factory/Dockerfile.cpu`. Passed in kind CI on main ([run 36990962579](https://github.com/zyvorai/gryvia/actions/runs/36990962579), 2026-10-02): base score 0.0, fine-tuned 0.75 after 3 epochs. With 3 epochs the score moved between 0.5 and 0.875 across nearby learning rates locally; with 5 it was 1.0 at every rate tried (7e-4, 1e-3, 1.5e-3), so the step trains 5 epochs and requires at least 0.875. Both evaluations also run lm-eval `arc_easy` (`--tasks arc_easy --limit 20`, dataset from huggingface.co); locally 0.40 before and 0.35 after, and the step requires the fine-tune to lose at most 0.25 and the score to be the mean of the two |

| Not verified anywhere | Why |
| --- | --- |
| Fine-tuning a model of useful size, `--qlora`, multi-GPU `torchrun`, and the bf16 path | Only a 135M model in float32 on CPU was run |
| A real quantization, lm-eval on full tasks, and multiple-choice tasks (`arc_easy`, `hellaswag`) through an endpoint | `quantize.py` (llm-compressor AWQ/GPTQ) needs a GPU. The CPU run uses 20 `arc_easy` questions on a local model directory. Through an endpoint, lm-eval scores multiple-choice tasks from the prompt's token logprobs (`echo`); llama.cpp's server does not return those (`KeyError: 'token_logprobs'`), vLLM does. Generation tasks such as `gsm8k` work with both |
| vLLM serving the quantized (compressed-tensors) weights, and the quality loss of AWQ on your model | No GPU; the evaluation step scores the quantized model so the promotion decision includes the loss |
| A token, gated models and rate limits on the real Hugging Face API | The real-API run is unauthenticated and lists one author's public models |
| vLLM loading a fine-tuned model from the registry PVC, and registry auto-serve with a real engine | No GPU. The real-engine run uses llama.cpp through a `GryviaInferenceService` with `image` and `healthCheck`; `servingConfig` has no image or health-check fields, so `autoServe` still uses the backend's default image |
| The GPU estimate being enough memory | It is a rule of thumb (below), not a measurement |

The [GPU-cluster runbook](gpu-ai-runbook.md) is the procedure for verifying these rows on a cluster with GPUs.

## Turn it on

```bash
helm upgrade gryvia helm/gryvia --reuse-values --set aiOperator.modelWatch.enabled=true
```

| Chart value | Flag | Default | Meaning |
| --- | --- | --- | --- |
| `aiOperator.modelWatch.enabled` | `--enable-model-watch` | `false` | Run the model watch controller (needs the ML controllers, which are on by default) |
| `aiOperator.modelWatch.hubURL` | `--model-watch-hub-url` | `https://huggingface.co` | Hub base URL; every watch uses it, tenants cannot pick another |
| `aiOperator.modelWatch.minPollInterval` | `--model-watch-min-poll-interval` | `5m` | Shortest `pollInterval` honoured |

The operator makes outbound HTTPS requests to the hub. A watch's `tokenSecretRef` Secret (same namespace) is sent as a
bearer token to that URL only.

## GryviaModelWatch

| Spec | Behaviour |
| --- | --- |
| `sources[]` | `provider: huggingface` (`ngc` is accepted by the schema but reported as unsupported), `author`, `nameRegex` (Go RE2 on the part after `author/`), `pipelineTag`, `minDownloads`, `limit` (newest models per poll, default 20, at most 100) |
| `licenseAllowlist` | SPDX-style ids from the hub's `license:` tag. Empty allows every license; a model without a license tag is rejected when the list is set |
| `maxParamsB` | Upper bound in billions. Size comes from the safetensors metadata, else the name (`7B`, `0.5B`, `135M`, `8x7B`); an unknown size is rejected when this is set |
| `tokenSecretRef` | `{name, key}` (key default `token`). Without it, gated models are rejected |
| `pollInterval` | Default `1h`, at least `--model-watch-min-poll-interval` |
| `includeExisting` | Off: the first poll records what exists as `Baseline` and only later releases run |
| `retrainOnNewRevision` | Off: a model runs once. On: a new commit of a model already run starts another run |
| `maxConcurrentRuns` | Default 1; queued candidates start oldest first |
| `sizing` | `gpuMemoryGB` (80), `overheadPercent` (150), `maxGPUs` (8), `autoSizeSteps`: job steps in this list with `gpus: 0` get the estimate |
| `suspend` | Stops polling; running workflows continue |
| `workflowTemplate` | A `GryviaWorkflowSpec` rendered per model (below) |

**GPU estimate**: parameters × 2 bytes (bf16) × `overheadPercent`/100 ÷ `gpuMemoryGB`, rounded up to 1, 2, 4, 8 and
then multiples of 8, capped by `maxGPUs` (a model needing more is rejected). Unknown size: 1. The 150% default leaves
room for LoRA activations and optimizer state; full fine-tuning needs far more.

**Template placeholders**, filled when the run is created: `{{model.id}}` (`Qwen/Qwen3-8B`), `{{model.name}}`
(`Qwen3-8B`), `{{model.slug}}` (`qwen3-8b`, a DNS-safe name of at most 40 characters), `{{model.revision}}` (commit
SHA), `{{model.paramsB}}`, `{{model.license}}`, `{{model.gpus}}`. The workflow also gets the parameters `MODEL_ID`,
`MODEL_REVISION`, `MODEL_PARAMS_B` and `MODEL_GPUS` (unless the template sets them). Unknown placeholders are left for
the workflow (`{{steps...}}`, `{{parameters...}}`).

Runs are named `<watch>-<slug>-<revision[:7]>`, labelled `gryvia.io/model-watch=<watch>`, annotated with
`gryvia.io/source-model` and `gryvia.io/source-revision`, and owned by the watch (deleting the watch deletes them;
registry entries are not owned by runs and stay).

Status: `phase` (`Watching`, `Suspended`, `Failed` for an invalid spec), conditions `Valid` and `HubReadable`,
`lastPollTime`, `nextPollTime`, `activeRuns`, `message`, and `candidates[]` (at most 200, oldest finished ones dropped
first) with `id`, `revision`, `paramsB`, `license`, `gpus`, `phase` (`Queued`, `Running`, `Succeeded`, `Failed`,
`Rejected`, `Baseline`), `workflow`, `firstSeen`, `message` (why it was rejected or failed).

## Workflow additions

These work in any `GryviaWorkflow`, not only in model watch runs.

**Step outputs.** When a `job` or `script` step succeeds, its outputs are read from the termination message of the
succeeded pod (completion index 0 for jobs): a JSON object of string, number or boolean values. For job steps, the
annotations `gryvia.io/output-<key>` on the `GryviaAIJob` are read too and win. Keys match `[A-Za-z0-9_-]{1,63}`,
values are at most 4096 bytes, at most 32 per step. They are written to `status.stepStatuses[].outputs`.
`examples/model-factory/outputs.py` writes them.

**Templating.** Before a step starts, `{{steps.<step>.outputs.<key>}}`, `{{parameters.<name>}}`, `{{workflow.name}}` and
`{{workflow.run}}` are replaced in its payload. A placeholder that cannot be resolved (an output the step never
reported) fails the step with a message instead of running it with a literal `{{...}}`.

**`register` step.** Creates a `GryviaModelRegistry` entry (`name` default `<workflow>-<step>`) from `modelName`,
`version`, `artifacts`, `stage` (`dev` or `staging`; promotion to production is the policy's job), `description`,
`metadata`, `autoServe`, `servingConfig` and `promotionPolicy`. `stage` defaults to `staging`, the stage the promotion
policy judges; `dev` keeps the entry out of automatic promotion. The entry gets `source.workflowRef`, the metadata
`base_model`/`base_revision` from a model watch run, and the step output `name`. An existing entry with that name that
this run did not create fails the step (no overwrite). Entries are not owned by the workflow.

**`registry` step.** Changes an existing `GryviaModelRegistry` entry in the workflow's namespace: `entry` names it,
or `serviceName` picks the entry a `GryviaInferenceService` serves when the step runs (its `modelRef`; a shared service or one created by hand). `action: updateMetadata`
merges `metadata` into `spec.metadata` (values may use placeholders, for example `{{steps.evaluate.outputs.score}}`);
`action: rollback` asks the registry controller for a rollback (below). The step output `name` is the entry. A missing
entry or service fails the step.

**`schedule`.** A 5-field cron expression in UTC. The workflow waits in phase `Scheduled` (`status.nextScheduleTime`)
and then runs; each run starts from a clean status with `status.run` incremented, and children are named with an
`-n<run>` suffix. Runs never overlap: a fire time that passes during a run starts the next run as soon as it ends.

## Promotion policy and shared serving

```yaml
promotionPolicy: {metric: eval_score, direction: maximize, minDelta: "0.01", threshold: "0.5"}
```

On every reconcile of an entry in `staging` (entries in `dev`, `production` or `archived` are not judged): the metric is
read from `spec.metadata`. Missing
is `Waiting`, not a number is `Rejected`, below `threshold` is `Rejected`. The entry is compared with the best
production entry of the same `modelName` in the namespace; it is promoted (`spec.stage: production`) if it improves on
it by more than 0 and by at least `minDelta`, or if there is no production entry. A production entry without a numeric
metric makes the decision `Rejected` ("promote by hand"). The outcome is in `status.promotionDecision` and the
condition `PromotionGate`.

`servingConfig.serviceName` turns on shared serving for production entries with `autoServe`:

* No service of that name: it is created (labelled `gryvia.io/serving-group=<name>`, **not owned** by the entry, so
  archiving the first version does not delete it) with `healthCheck.autoRollback`.
* The entry is the service's `modelRef`: its settings are kept in sync and its status mirrors the service.
* Otherwise the entry is patched in as the canary (`autoPromote`, `canaryWeight` default 10, `promoteAfterSeconds`
  default 300) unless another version is still a canary (it waits). The inference controller promotes or rolls back
  the canary as described in [ML controllers](ml-controllers.md). On promotion the service's `modelRef` becomes the
  entry, the replaced entry is archived and recorded in `status.previousVersion`; on rollback the entry's phase is
  `RolledBack` and it is not tried again.

Both the stable and the canary Deployment load the artifacts of their own version's registry entry.

`servingConfig.servicePort` is the port the serving container is served and probed on. Set it whenever `args` move the
server off its backend default (vLLM and Triton 8000, TorchServe 8080); for example vLLM with `--port=8080` needs
`servicePort: 8080`, otherwise the pods never pass their probes.

## Quantization

An optional step between fine-tune and evaluate turns the model into 4-bit AWQ or GPTQ weights
(`examples/model-factory/quantize.py`), so the evaluation scores the quantized model. See
[Model evaluation](model-evaluation.md#quantization).

## Continuous evaluation and rollback

A scheduled workflow re-scores the served version and a `registry` step writes the score onto its entry; the
registry's `rollbackPolicy` brings the previous version back when the score crosses the threshold, and
`POST /api/models/{name}/rollback` or `gryvia models rollback` does it on request. See
[Model evaluation](model-evaluation.md).

## Surfaces

* Gateway: `GET/POST /api/model-watches`, `GET /api/model-watches/{name}`, `GET /api/model-watches/{name}/runs`
  (newest first), `POST /api/model-watches/{name}/suspend|resume`, `DELETE /api/model-watches/{name}`;
  `POST /api/models/{name}/rollback` (202; 409 when the entry cannot be rolled back).
* CLI: `gryvia models watch list|runs|create -f|suspend|resume|delete`, `gryvia models rollback <entry>`.
* Dashboard: **Models → Model factory** lists watches and the runs of the selected one.

## RBAC

The operator ClusterRole gains `gryviamodelwatches` (with `/status` and `/finalizers`). The workflow controller gains
create and patch on `gryviamodelregistries` (register and registry steps) and get on `gryviainferenceservices`
(`registry` steps with `serviceName`), the registry controller get/list/watch on `apps/deployments` (to read canary
decisions) and create on `events`. Reading the token Secret uses the secrets access the ClusterRole already had. The gateway and
the tenant roles get the same verbs on `gryviamodelwatches` as on `gryviaautotuners`.
