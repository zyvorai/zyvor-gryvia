# Model evaluation, rollback and quantization

The model factory scores a model before it is promoted. This page covers the checks that happen around a promoted
version: scheduled re-evaluation of the served version, rollback when its score drops, and 4-bit quantization of a
fine-tuned model before it is evaluated and served. The watch, the workflow and the promotion policy are described
in [Model factory](model-factory.md).

## What is verified and what is not

| Verified | How |
| --- | --- |
| `registry` workflow steps (`updateMetadata` by entry name or by `serviceName`, `rollback`), validation and a missing entry | Fake-client tests `TestWorkflow_RegistryStep*` in `operators/ai-operator/controllers/gryviamodelregistry_rollback_test.go` |
| `rollbackPolicy` (maximize and minimize, within threshold, missing or non-numeric metric), requested rollbacks, refusals (`NotServing`, `NoPreviousVersion`, `NotStable`), the shared service's `modelRef` and canary, archiving, condition and Events | `TestModel_Rollback*` and `TestRollbackBreach` in the same file |
| The whole loop on a real API server: a scheduled-evaluation workflow (busybox evaluation reporting 0.3) writes `live_score` onto the served entry, the policy breaches, the service goes back to the previous version, which serves again; the gateway refuses to roll back the archived entry | The "Model factory - a scheduled evaluation drops the score…" step of `.github/workflows/e2e-ml.yml` (kind, CPU only); these steps also passed on a single-node k3s host (2026-10-01) |
| `quantize.py` arguments (method, scheme, samples, output path), calibration text rendering; the quantize step's outputs flowing into the register step | `examples/model-factory/test_model_factory.py`; the e2e's busybox quantize step |
| Gateway and CLI rollback | `services/api-gateway/tests/test_models.py`, `cli/src/commands/models.rs` |

| Not verified | Why |
| --- | --- |
| A real evaluation (`evaluate.py` with lm-evaluation-harness or against a live endpoint) | No GPU or model in CI; the e2e evaluation step only reports a fixed score |
| A real AWQ or GPTQ quantization, vLLM loading compressed-tensors weights, and the quality loss on your model | No GPU in CI; the script calls llm-compressor as documented but has not run it |

## Scheduled evaluation

A promoted version can get worse in production (data drift, a serving regression). A scheduled `GryviaWorkflow`
re-scores the served version, and a `registry` step writes the score onto its entry:

```yaml
steps:
  - name: evaluate
    type: job                       # or script
    # ... evaluate.py --endpoint http://<service>:8000 writes {"score": "0.71"} to its termination message
  - name: record
    type: registry
    dependsOn: [evaluate]
    registry:
      serviceName: chat             # the entry this shared service serves (or entry: <name>)
      action: updateMetadata
      metadata: {live_score: "{{steps.evaluate.outputs.score}}"}
```

[eval-schedule.yaml](../examples/model-factory/eval-schedule.yaml) runs this every night (`spec.schedule`). The step
outputs `name` (the entry it wrote to).

## Rollback

```yaml
rollbackPolicy: {metric: live_score, threshold: "0.5", direction: maximize}
```

The registry controller rolls the shared service back when the entry it serves has a `rollbackPolicy` and
`spec.metadata[metric]` is below `threshold` (`maximize`, the default) or above it (`minimize`). It also rolls back
when the entry has the annotation `gryvia.io/rollback-requested`, which is set by `POST /api/models/{name}/rollback`,
`gryvia models rollback` and `registry` steps with `action: rollback`:

1. The entry in `status.previousVersion` is moved back to `production`. This is an entry name, or a version of the
   same `modelName`, on the same `serviceName`.
2. The service's `modelRef` becomes that entry and any canary is dropped.
3. The rolled-back entry is archived. It keeps phase `RolledBack` and gets the condition `RolledBack=True` (reason
   `PolicyBreached` or `Requested`) and an Event `RolledBack`. The annotation is removed.

A missing or non-numeric metric is not a breach. The policy only acts on the entry the service serves, or on a
canary, which is then archived.

Some requested rollbacks cannot be done: the entry is not an auto-served production entry with a `serviceName`, or
there is no previous version on that service. These set `RolledBack=False` with the reason (`NotServing`,
`NoPreviousVersion`, `NotStable`) and an Event `RollbackRefused`, remove the annotation and change nothing else. From
then on, the restored version is judged by its own `rollbackPolicy` and metadata.

## Quantization

`examples/model-factory/quantize.py` turns the fine-tuned model into 4-bit weights with
[llm-compressor](https://github.com/vllm-project/llm-compressor). There are two methods:
- `--method awq` (the default), with default scheme `W4A16_ASYM`.
- `--method gptq`, with `W4A16` (also `W8A16`).

Calibration uses `--samples` (256) records of the training JSONL, rendered with the chat template. `lm_head` stays
in full precision. The output goes next to the input (`<path>-awq`) in the compressed-tensors format. The step
reports `path`, `subPath`, `format` and `quantization` (`compressed-tensors`), `method` and `scheme`.

In [model-watch.yaml](../examples/model-factory/model-watch.yaml) the quantize step runs between fine-tune and
evaluate, so the evaluation scores the quantized model and the promotion decision includes the quality loss. The
register step stores `artifacts.format`, `metadata.quantization` and the vLLM argument
`--quantization={{steps.quantize.outputs.quantization}}`. 4-bit weights need about a quarter of the GPU memory of
bf16, so the same `gpuCount` serves a larger model or longer contexts. No CRD change is involved: it is an ordinary
job step.
