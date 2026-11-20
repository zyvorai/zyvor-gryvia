## External System Integrations

How to use Gryvia together with other ML and platform tools.

:::caution There are no built-in connectors
Gryvia has **no integration controller and no `integrations:` field** on `GryviaAIJob`. It does not talk to Weights &
Biases, Neptune, MLflow, HuggingFace, Airflow, Datadog, Slack, LDAP and so on by itself, and it does not log anything to
those services for you. Earlier versions of this page described such features; they never existed. What is real:

- A `GryviaAIJob` runs your container. Anything your training code does (log to W&B, push to the Hub) works if the
  container has network access and the credentials, which you pass with `spec.env` and Kubernetes Secrets.
- External tools can create and read jobs through the [gateway REST API](../developer-guide/api-reference.md), the
  [CLI](./CLI_GUIDE.md) (which uses your kubeconfig), the Python SDK (REST) or the Go SDK (Kubernetes API).
- Metrics are exposed for Prometheus (DCGM exporter, operator metrics) and can be scraped by whatever you run.
- Sign-in through an OIDC identity provider is implemented in the gateway ([Authentication and TLS](./AUTH_AND_TLS.md)).

The recipes below are ordinary Kubernetes usage. None has been tested against the third-party service it mentions.
:::

A few kinds do reach outside the cluster, all opt-in: `GryviaJobHook` posts a webhook when a job or workflow changes
phase ([job hooks](https://github.com/zyvorai/gryvia/blob/main/docs/job-hooks.md)), `GryviaDataset` downloads http,
s3, nfs or Hugging Face data ([datasets](https://github.com/zyvorai/gryvia/blob/main/docs/datasets.md)),
`GryviaModelWatch` polls the Hugging Face Hub, and budget alerts can go to one webhook. See
[reference/crds.md](../reference/crds.md) for which kinds have a controller.

## Experiment Tracking

### Weights & Biases

Put the key in a Secret and hand it to your training process as an environment variable. The logging itself is done by
your script (`wandb.init(...)`); Gryvia does not add GPU metrics, cost or job metadata to your W&B run.

```bash
kubectl create secret generic wandb-api-key --from-literal=api-key=YOUR_API_KEY
```

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: training-with-wandb
spec:
  type: training
  gpus: 1
  gpuType: any
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command: ["python", "train.py", "--wandb-project=my-project"]
  env:
    - name: WANDB_API_KEY
      valueFrom:
        secretKeyRef:
          name: wandb-api-key
          key: api-key
    - name: WANDB_ENTITY
      value: my-team
```

### Neptune, MLflow and TensorBoard

The same pattern applies: pass the token or tracking URI as `spec.env` (for example `NEPTUNE_API_TOKEN`,
`MLFLOW_TRACKING_URI`) and log from your code. Gryvia does not create a TensorBoard Service; run TensorBoard yourself
against a shared volume (`spec.storage` gives a job a PVC mounted at `/data`, see the
[platform setup guide](./COMPLETE_DEPLOYMENT_GUIDE.md)) and expose it with your own Deployment and Service.

## Model Registries

### HuggingFace Hub and other registries

Do the upload at the end of your training script, or as a follow-up Kubernetes Job you create yourself, with the token
from a Secret (`kubectl create secret generic hf-token --from-literal=token=YOUR_HF_TOKEN`).
A job hook can tell your own automation when training finished, so it runs the push (the hook only sends the
notification; the receiver does the work):

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaJobHook
metadata:
  name: push-to-huggingface
spec:
  events: [Succeeded]
  kinds: [GryviaAIJob]
  selector:
    matchLabels:
      publish: huggingface
  webhook:
    url: https://automation.example.com/push-model
    secretRef:
      name: hook-secret
```

`GryviaModelRegistry` tracks versions, promotion and serving inside the cluster (see [ML Workflows](./ML_WORKFLOWS.md)).
There is no SageMaker or MLflow registry integration.

## Data Platforms

`GryviaDataset` (opt-in, `storageOperator.datasets.enabled`) downloads an http, s3, nfs or Hugging Face source into a
versioned directory on a PVC that jobs mount ([datasets](https://github.com/zyvorai/gryvia/blob/main/docs/datasets.md)).
For anything else, use what you already use (DVC, Pachyderm, object storage) inside the job container or an init
container, with credentials from Secrets, and a PVC or CSI volume for shared data
(see [Storage and Network Operators](./STORAGE_NETWORK_OPERATORS.md)).

## Workflow Orchestration

Any orchestrator that can run `kubectl` or the Gryvia CLI in a container with cluster credentials can create jobs. There
is no published Gryvia CLI container image; the release provides CLI binaries (`gryvia-<tag>-<os>-<arch>` on the
GitHub release), so build a small image that contains the binary and mount a kubeconfig or use an in-cluster service
account.

### Airflow

```python
from airflow import DAG
from airflow.providers.cncf.kubernetes.operators.pod import KubernetesPodOperator

with DAG('ml_training', schedule='@daily') as dag:
    train = KubernetesPodOperator(
        task_id='train_model',
        namespace='default',
        name='submit-job',
        image='registry.example.com/your-org/gryvia-cli:latest',   # your own image containing the gryvia binary
        cmds=['gryvia'],
        arguments=['submit', '--file', '/jobs/job.yaml', '--wait'],
    )
```

Kubeflow Pipelines and Argo work the same way: run `gryvia submit --file job.yaml --wait`, or `kubectl apply`, or call
`POST /api/jobs` on the gateway from a step.

## Monitoring and Observability

Gryvia does not push metrics to Datadog, New Relic or Grafana Cloud, and it does not define `gryvia_gpu_utilization`,
`gryvia_job_duration` or `gryvia_cost_total` Prometheus metrics. What exists:

- The DCGM exporter (port 9400) on GPU nodes and the operators' controller-runtime metrics endpoints, scrapeable by any
  Prometheus. `helm/observability` wraps `kube-prometheus-stack`; `monitoring/` has a ServiceMonitor, alert rules and
  dashboards as templates.
- Gateway routes that read Prometheus when `apiGateway.prometheusUrl` is set (`/api/metrics/gpu` and cost history).
  The gateway itself has no `/metrics` endpoint.
- To forward metrics to a SaaS, use that vendor's Prometheus scraper or remote-write against the Prometheus you run.

## CI/CD Integration

The CLI talks to the Kubernetes API with a kubeconfig, so a CI runner needs the `gryvia` binary and a kubeconfig for a
service account that may create `GryviaAIJob` objects.

### GitHub Actions

```yaml
name: Train Model
on:
  push:
    branches: [main]

jobs:
  train:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Install gryvia
        run: |
          # download the release binary for linux-amd64 and put it on PATH (see the GitHub release page)
          echo "install gryvia here"

      - name: Submit training job
        run: gryvia submit --file job.yaml --wait
        env:
          KUBECONFIG: ${{ github.workspace }}/kubeconfig   # written from a secret in an earlier step

      - name: Job status
        run: gryvia status training-job
```

GPU type and count are set in `job.yaml` (`spec.gpuType`, `spec.gpus`); `gryvia submit` has no `--gpu-type` or
`--gpu-count` flag. `--wait` waits for completion and `--logs` follows logs. Adapt the same commands for GitLab CI or
Jenkins (`gryvia submit --file job.yaml --wait`, `gryvia logs training-job`).

## Notifications

### Webhooks that exist today

The network-intelligence CRDs accept a webhook URL that the operator calls: `alertWebhook` on `GryviaNetworkAnomaly` and
`GryviaSecurityPolicy` (see [Network Intelligence](./NETWORK_INTELLIGENCE.md); those controllers need Cilium and a
working collector path). Point it at a Slack incoming webhook or any HTTP receiver. Job notifications come from job
hooks (below). There is no Slack bot and no `/gryvia` slash command.

### Job hooks

With `aiOperator.jobHooks.enabled`, a `GryviaJobHook` in a namespace posts a message when a `GryviaAIJob` or
`GryviaWorkflow` there reaches one of its events. `format: slack` sends `{"text": ...}` for an incoming webhook:

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaJobHook
metadata:
  name: slack-notifications
spec:
  events: [Failed, Succeeded]
  webhook:
    url: https://hooks.slack.com/services/YOUR/WEBHOOK/URL
    format: slack
```

Delivery is at least once with retries; see [job hooks](https://github.com/zyvorai/gryvia/blob/main/docs/job-hooks.md)
for the payload, signature and the addresses the operator refuses.

## Cost Management

Cost estimates come from the quota operator (`GryviaCostPredictor`, `GryviaUsageRecord`) and the SKU catalog:
`gryvia cost`, `gryvia usage`, `gryvia invoice`, `gryvia catalog`, and the gateway's `/api/metrics/costs` and
`/api/usage/*` routes. There is no CloudHealth or Kubecost connector. To export usage, use `GET /api/usage/export`
(see the [API reference](../developer-guide/api-reference.md)).

## Authentication

OIDC sign-in is implemented in the API gateway; LDAP is not supported. Configure OIDC through the gateway environment
and the chart values `apiGateway.oidc.*`, and map users to tenants with `GryviaTenant`. See
[Authentication and TLS](./AUTH_AND_TLS.md) and [GPU as a Service](./GPU_AS_A_SERVICE.md). Payments and a real identity
provider have not been exercised end to end.

## Backup and DR

State lives in custom resources; see [Operations](./OPERATIONS.md) for export, restore (`scripts/backup-crs.sh`) and upgrade.
A generic Velero schedule is a possible addition (not tested with Gryvia). Many Gryvia kinds are cluster-scoped, so
include cluster resources:

```yaml
apiVersion: velero.io/v1
kind: Schedule
metadata:
  name: gryvia-backup
  namespace: velero
spec:
  schedule: "0 2 * * *"
  template:
    includedNamespaces:
      - gryvia-system
    includeClusterResources: true
    includedResources:
      - gryviaaijobs.gryvia.io
      - gryviaquotas.gryvia.io
      - gryviatenants.gryvia.io
      - persistentvolumeclaims
```

There is no `GryviaJobHook`-based checkpoint sync; use your own sidecar or script (`aws s3 sync` and similar) to copy
checkpoints, or the checkpoint features of your framework.

## Security Scanning

Image scanning is independent of Gryvia. For example, a plain Kubernetes CronJob running Trivy against an image you use:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: image-scanning
spec:
  schedule: "0 */6 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: trivy
              image: aquasec/trivy:0.58.0
              args: ["image", "--severity=HIGH,CRITICAL", "nvcr.io/nvidia/pytorch:24.01-py3"]
```

Release images are signed with cosign in the release workflow.

## SDKs

### Python SDK

An async REST client for the gateway (jobs, nodes, quotas, costs, metrics). Install from source
(`cd sdk/python && pip install -e .`); publication to PyPI is not verified. It has unit tests against mocked HTTP only.
It does not cover SKUs, tenants, usage, invoices or Flight Recorder, and `Jobs.stream_logs` does not work against the
current gateway (see `sdk/python/README.md`).

```python
import asyncio
from gryvia import Gryvia

async def main():
    async with Gryvia(api_url="https://gryvia.example.com", token="your-api-token") as tr:
        job = await tr.jobs.create({
            "apiVersion": "gryvia.io/v1alpha1",
            "kind": "GryviaAIJob",
            "metadata": {"name": "my-training"},
            "spec": {
                "type": "training",
                "image": "nvcr.io/nvidia/pytorch:24.01-py3",
                "gpus": 4,
                "gpuType": "A100-80G",
                "command": ["torchrun", "--nproc_per_node=4", "train.py"],
            },
        })
        final = await tr.jobs.wait_for_completion("my-training", timeout=7200)
        print(final.status.phase)

asyncio.run(main())
```

### Go SDK

`github.com/zyvorai/gryvia/sdk/go` is a typed controller-runtime client for the CRDs (it uses the Kubernetes API, not
the gateway). It wraps create/get/list/delete for jobs and several other kinds (whether a controller acts on those
kinds is a separate question, see above).

```go
import (
    sdk "github.com/zyvorai/gryvia/sdk/go"
    "sigs.k8s.io/controller-runtime/pkg/client/config"
)

cfg, _ := config.GetConfig()
c, _ := sdk.NewClient(cfg)

job := &sdk.GryviaAIJob{}
job.Name = "my-training"
job.Namespace = "ml-training"
job.Spec.Type = "training"
job.Spec.Image = "nvcr.io/nvidia/pytorch:24.01-py3"
job.Spec.GPUs = 8

err := c.CreateJob(ctx, job)
```

Check `sdk/go/client.go` and the CRD types in `operators/ai-operator/api/v1` for exact field names.

## REST API

Gateway routes live under `/api/` (there is no `/api/v1` prefix) and take `Authorization: Bearer <token>`; the
[API reference](../developer-guide/api-reference.md) lists every route and its access rule. Creating a job:

```bash
curl -k -X POST https://<dashboard-host>/api/jobs \
  -H "Authorization: Bearer $GRYVIA_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "gryvia.io/v1alpha1",
    "kind": "GryviaAIJob",
    "metadata": {"name": "training-job"},
    "spec": {
      "type": "training",
      "image": "nvcr.io/nvidia/pytorch:24.01-py3",
      "gpus": 8,
      "gpuType": "A100-80G",
      "command": ["python", "train.py"]
    }
  }'

curl -k https://<dashboard-host>/api/jobs/training-job -H "Authorization: Bearer $GRYVIA_API_KEY"
curl -k "https://<dashboard-host>/api/jobs/training-job/logs?tail=100" -H "Authorization: Bearer $GRYVIA_API_KEY"
```

The gateway sets the namespace itself. There is no GraphQL API.

## Best Practices

### Secret management

Use Kubernetes Secrets for API keys and tokens, and reference them with `secretKeyRef`:

```bash
kubectl create secret generic wandb-api-key --from-literal=api-key=YOUR_API_KEY
kubectl create secret generic hf-token --from-literal=token=YOUR_HF_TOKEN
```

### Least privilege for automation

Give CI and orchestrators a dedicated service account with only the rights to create and read `gryvia.io` jobs in the
namespaces they need, or an OIDC tenant identity when going through the gateway, rather than the admin API key.

## Troubleshooting

```bash
# Platform and component status
gryvia status
kubectl get pods -n gryvia-system

# Why a job did not start
gryvia get job <name>
kubectl describe gryviaaijob <name>
kubectl logs -n gryvia-system deploy/gryvia-ai-operator

# A secret referenced by a job
kubectl get secret wandb-api-key
```

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
- Discussions: https://github.com/zyvorai/gryvia/discussions

---

*See [ADVANCED_FEATURES.md](ADVANCED_FEATURES.md) for more information*
