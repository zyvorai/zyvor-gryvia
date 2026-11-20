# ML Workflow Examples

Example YAML manifests for Gryvia ML workflow CRDs.

> **Status.** All five kinds (`GryviaAutoTuner`, `GryviaWorkflow`, `GryviaModelRegistry`, `GryviaInferenceService`, `GryviaWorkspace`) are run by the ai-operator's ML controllers (on by default, `--enable-ml-controllers`), so applying these manifests creates real pods, PVCs, Services and child jobs. They are written for GPU clusters (A100s, large images) and have not been run as written; the e2e (`e2e-ml.yml`) uses tiny CPU images. See [ML controllers](../../docs/ml-controllers.md) for what each controller does.

## Examples

- **[hyperparameter-tuning.yaml](hyperparameter-tuning.yaml)** - `GryviaAutoTuner` running Bayesian HPO for ResNet with 50 trials
- **[training-pipeline.yaml](training-pipeline.yaml)** - `GryviaWorkflow` with a 4-step DAG: preprocess, train, evaluate, deploy
- **[model-registry.yaml](model-registry.yaml)** - `GryviaModelRegistry` registering a trained model with artifacts and metadata
- **[inference-service.yaml](inference-service.yaml)** - `GryviaInferenceService` deploying vLLM with canary traffic splitting (90/10)
- **[gpu-workspace.yaml](gpu-workspace.yaml)** - `GryviaWorkspace` creating a JupyterLab environment with 2 A100 GPUs

## Usage

```bash
kubectl apply -f hyperparameter-tuning.yaml
kubectl apply -f training-pipeline.yaml
kubectl apply -f model-registry.yaml
kubectl apply -f inference-service.yaml
kubectl apply -f gpu-workspace.yaml
```
