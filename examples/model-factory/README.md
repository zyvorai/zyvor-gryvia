# Model factory: fine-tune and serve new open models automatically

A `GryviaModelWatch` polls the Hugging Face Hub. For each new model that passes its filters it runs a
`GryviaWorkflow`: download the weights to a shared PVC, LoRA fine-tune them on your data, evaluate, and register the
result in the `GryviaModelRegistry`. The registry's `promotionPolicy` moves the version to production only if its
evaluation score beats the version being served, and `servingConfig.serviceName` rolls it out as a canary of the
shared vLLM service, promoted after it stays healthy or rolled back.

> **Status.** The controllers are unit-tested and the control-plane flow (e2e with a stand-in hub and busybox steps) passed on a k3s cluster without GPUs and in kind CI.
> The download, fine-tune, evaluate (with lm-eval `arc_easy`) and GGUF conversion scripts ran for real on CPU in the
> kind e2e (SmolLM2-135M, image [Dockerfile.cpu](Dockerfile.cpu)), and llama.cpp served the result from the registry
> PVC; quantization and anything on GPUs or with a real vLLM image have not run. See [docs/model-factory.md](../../docs/model-factory.md); the steps to verify it on GPUs are in
> [docs/gpu-ai-runbook.md](../../docs/gpu-ai-runbook.md).

## Files

- [model-watch.yaml](model-watch.yaml): the watch, its token Secret and the five-step workflow template (download, fine-tune, quantize, evaluate, register)
- [eval-schedule.yaml](eval-schedule.yaml): a nightly workflow that scores the served version through its endpoint
  and writes `live_score` onto its entry; the entries' `rollbackPolicy` brings the previous version back below 0.5
- [download.py](download.py): `snapshot_download` of one revision into `/models/cache`, skipped when already present
- [finetune_lora.py](finetune_lora.py): TRL/PEFT LoRA (or `--qlora`) fine-tune; merges the adapter and saves safetensors
- [quantize.py](quantize.py): AWQ or GPTQ 4-bit weights with llm-compressor, calibrated on the training data; saves
  compressed-tensors that vLLM loads with `--quantization=compressed-tensors`
- [evaluate.py](evaluate.py): lm-evaluation-harness tasks plus an optional exact-match JSONL, on a model directory or
  (`--endpoint`) an OpenAI-compatible server; reports `score`. With `--endpoint --tasks`, lm-eval tokenizes locally:
  pass `--tokenizer` (the model directory or hub ID) unless the served model name is a hub ID. Multiple-choice tasks
  need prompt logprobs (vLLM); llama.cpp's server only supports generation tasks such as `gsm8k`
- [convert_gguf.py](convert_gguf.py): llama.cpp's `convert_hf_to_gguf.py` on a merged model; writes `model-<outtype>.gguf`
  next to the safetensors for `llama-server -m /models/model-<outtype>.gguf`
- [outputs.py](outputs.py): writes step outputs to the termination message the workflow reads
- [Dockerfile](Dockerfile) and [requirements.txt](requirements.txt): one image for the download, fine-tune, quantize and evaluate steps
- [Dockerfile.cpu](Dockerfile.cpu): a CPU image with pinned versions for download, fine-tune (`--cpu`) and evaluate with tiny models
- [test_model_factory.py](test_model_factory.py): unit tests (no GPU, no network)

## Run it

```bash
docker build -t registry.example.com/gryvia-model-factory:1.0 examples/model-factory
docker push registry.example.com/gryvia-model-factory:1.0

helm upgrade gryvia helm/gryvia --reuse-values --set aiOperator.modelWatch.enabled=true

# PVCs "models" (ReadWriteMany) and "datasets" (with chat.jsonl and eval.jsonl) must exist in ml-team.
kubectl apply -f examples/model-factory/model-watch.yaml
gryvia models watch runs open-llms -n ml-team      # or: kubectl get gryviamodelwatch open-llms -n ml-team -o yaml
kubectl get gryviaworkflows,gryviamodelregistries,gryviainferenceservices -n ml-team

# Nightly re-evaluation of the served version, and a manual rollback.
kubectl apply -f examples/model-factory/eval-schedule.yaml
gryvia models rollback <serving-entry> -n ml-team
```

The first poll only records the models that already exist (`Baseline`); set `includeExisting: true` to fine-tune
them too. Each step's outputs are visible in `kubectl get gryviaworkflow <run> -o jsonpath='{.status.stepStatuses}'`.

The serving args assume the image's entrypoint is the vLLM OpenAI server (`--model`, `--port`); check the
entrypoint of the vLLM image you configure with `--inference-image-vllm`.

## Test the scripts

```bash
cd examples/model-factory && python3 test_model_factory.py
```
