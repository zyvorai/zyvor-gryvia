# Model Lineage

Track complete model provenance, compliance, and reproducibility for AI/ML models.

> **Status: partly implemented (ai-operator controller registered).** The controller collects
> provenance from referenced `GryviaAIJob`s, computes a SHA-256 provenance hash (chained to the previous
> hash when `cryptographicChain` is set) and sets a simple `complianceStatus` by checking that code
> commit, job reference and dataset checksums are present. Limits: the hash is a self-computed integrity
> value, not a signature or external attestation; `immutableRecord` is a field the controller does not
> enforce; `attachToRegistry` only sets a status flag (nothing is written to a `GryviaModelRegistry`
> entry, and the registry controller does not read lineage); the "regulatory framework" check is a completeness check, not a
> compliance certification. Unverified in production.

## Overview

GryviaModelLineage provides end-to-end tracking of how a model was produced:

- **Model Identity**: Name, version, registry, and format tracking
- **Code Provenance**: Git repo, commit, branch, and container image
- **Data Provenance**: Dataset references with versions and checksums
- **Training Provenance**: Job reference, hyperparameters, distributed configuration
- **Infrastructure Provenance**: GPU nodes, network type, storage backend, cost
- **Evaluation**: Metrics, benchmarks, evaluation job references
- **Compliance**: Immutable records, cryptographic hash chains, regulatory frameworks
- **Auto-Collection**: Automatically gather provenance from referenced GryviaAIJob resources

## Quick Start

### Create a Model Lineage Record

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaModelLineage
metadata:
  name: llama-70b-finetune-v1
  namespace: ml-training
spec:
  model:
    name: llama-70b-domain-adapted
    version: "1.0.0"
    registry: "registry.company.com/models/llama-70b-domain"
    format: safetensors

  provenance:
    autoCapture: true
    code:
      gitRepo: "https://github.com/company/llm-training"
      gitCommit: "a1b2c3d4e5f6"
      gitBranch: "main"
      containerImage: "registry.company.com/training:v1.2.0"
    data:
      datasets:
        - datasetRef: "domain-corpus-v3"
          version: "3.1.0"
          checksum: "sha256:abcdef1234567890"
    training:
      jobRef: "llama-70b-finetune-job"
      hyperparameters:
        learning_rate: "2e-5"
        batch_size: "32"
        epochs: "3"

  compliance:
    immutableRecord: true
    cryptographicChain: true
    signedBy: "ml-pipeline@company.com"
    regulatoryFramework:
      - EU-AI-Act
```

Apply:
```bash
kubectl apply -f model-lineage.yaml
```

Check status:
```bash
kubectl get gryviamodellineage -n ml-training
```

Output:
```
NAME                    MODEL                    VERSION  FORMAT       LINEAGE  COMPLIANCE  AGE
llama-70b-finetune-v1   llama-70b-domain-adapted 1.0.0   safetensors  true     Compliant   5m
```

## Auto-Collection

When `autoCapture: true` is set, the controller automatically collects provenance from the referenced GryviaAIJob:

- **Infrastructure**: GPU nodes used, network type, storage backend, GPU hours consumed
- **Training**: Distributed configuration summary (e.g., "8xH100, pytorch, NCCL over RDMA")
- **Code**: Container image from the job spec (if not manually specified)
- **Events**: Anomalies from job conditions, retry events

```yaml
provenance:
  autoCapture: true
  training:
    jobRef: "my-training-job"  # Controller will collect from this job
```

The controller watches for GryviaAIJob completions and automatically triggers re-collection when the referenced job finishes.

## Model Identity

Track model metadata:

```yaml
model:
  name: llama-70b-domain-adapted
  version: "1.0.0"
  registry: "registry.company.com/models/llama-70b-domain"
  format: safetensors  # pytorch | onnx | tensorrt | safetensors
```

## Provenance

### Code Provenance

```yaml
provenance:
  code:
    gitRepo: "https://github.com/company/llm-training"
    gitCommit: "a1b2c3d4e5f6"
    gitBranch: "main"
    containerImage: "registry.company.com/training:v1.2.0"
```

### Data Provenance

```yaml
provenance:
  data:
    datasets:
      - datasetRef: "domain-corpus-v3"
        version: "3.1.0"
        checksum: "sha256:abcdef1234567890"
      - datasetRef: "instruction-dataset"
        version: "2.0.0"
        checksum: "sha256:0987654321fedcba"
```

### Training Configuration

```yaml
provenance:
  training:
    jobRef: "llama-70b-finetune-job"
    hyperparameters:
      learning_rate: "2e-5"
      batch_size: "32"
      epochs: "3"
      warmup_steps: "500"
      weight_decay: "0.01"
    distributedConfig: "8xH100, FSDP, NCCL over RDMA"
```

### Infrastructure

```yaml
provenance:
  infrastructure:
    gpuNodes:
      - gpu-node-01
      - gpu-node-02
    networkType: rdma
    storageBackend: lustre-fast
    totalGpuHours: 384
    totalCost: 3072.00
```

### Events

```yaml
provenance:
  events:
    anomalies:
      - timestamp: "2024-03-15T10:30:00Z"
        type: "LossSpike"
        description: "Training loss spiked at epoch 2, step 5000"
    interventions:
      - timestamp: "2024-03-15T10:35:00Z"
        action: "ReducedLearningRate"
        reason: "Manual LR reduction after loss spike"
    checkpointTimeline:
      - timestamp: "2024-03-15T08:00:00Z"
        epoch: 1
        step: 10000
        checkpointPath: "/checkpoints/epoch-1"
```

### Evaluation

```yaml
provenance:
  evaluation:
    metrics:
      accuracy: 0.95
      f1: 0.93
      perplexity: 12.5
    evaluationJob: "llama-70b-eval-job"
    benchmarks:
      - name: "MMLU"
        score: 82.5
        date: "2024-03-16T00:00:00Z"
      - name: "HumanEval"
        score: 67.3
```

## Compliance

### Immutable Records

Prevent modifications after initial creation:

```yaml
compliance:
  immutableRecord: true
```

### Cryptographic Hash Chain

Enable SHA256 hash chain for tamper detection. Each lineage record's hash incorporates the previous hash:

```yaml
compliance:
  cryptographicChain: true
```

The controller computes `SHA256(previousHash + provenanceData)` and stores it in `status.provenanceHash`.

### Attestation

Generate and attach attestation to the model registry:

```yaml
compliance:
  attestation:
    format: in-toto        # in-toto | sigstore | custom
    attachToRegistry: true  # Attach to container/model registry
```

### Regulatory Framework

Declare compliance with regulatory frameworks:

```yaml
compliance:
  regulatoryFramework:
    - EU-AI-Act
    - NIST-AI-RMF
    - ISO-42001
```

The controller evaluates compliance based on:
- Code provenance completeness (git repo and commit)
- Training job reference
- Dataset checksums
- Data privacy assessment
- Model signing

### Data Privacy

```yaml
compliance:
  dataPrivacy:
    piiScanned: true
    piiFound: false
    dpiaCompleted: true
```

## Status

The controller updates the following status fields:

```yaml
status:
  lineageComplete: true           # All required provenance collected
  provenanceHash: "sha256:abc..."  # Cryptographic hash of provenance
  createdAt: "2024-03-15T08:00:00Z"
  attestationAttached: true       # Attestation attached to registry
  complianceStatus: Compliant     # Compliant | NonCompliant | Pending | Unknown
  reproducibilityVerified: true   # Can model be reproduced from provenance
```

### Compliance Status Values

- `Compliant` - All regulatory requirements met
- `NonCompliant` - One or more requirements not met
- `Pending` - Assessment in progress
- `Unknown` - No regulatory framework specified

### Reproducibility

A model is considered reproducible when:
- Git commit is recorded
- Container image is specified
- Hyperparameters are captured
- All dataset checksums are present

## Architecture

1. User creates a GryviaModelLineage CR with model identity and provenance
2. The controller watches for new lineage CRs
3. If `autoCapture` is enabled, it fetches the referenced GryviaAIJob and collects infrastructure, training, and event data
4. It computes a SHA256 hash chain for tamper detection
5. It evaluates compliance status against declared regulatory frameworks
6. It checks lineage completeness and reproducibility
7. When the referenced job completes, the controller re-reconciles to update provenance with final data

## Best Practices

1. Always specify `gitCommit` for exact reproducibility
2. Include dataset checksums (`sha256:...`) for data integrity verification
3. Enable `autoCapture` to reduce manual data entry
4. Set `cryptographicChain: true` for tamper-evident records
5. Declare `regulatoryFramework` early to get compliance feedback during development
6. Record all hyperparameters, even defaults, for full reproducibility
7. Use the `evaluation` section to track model quality alongside provenance

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
- Discussions: https://github.com/zyvorai/gryvia/discussions
