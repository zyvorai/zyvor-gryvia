# Idle autostop and reclaim

How Gryvia frees GPU capacity when workspaces, jobs or inference services go idle.

| Surface | Mechanism | Status |
|---|---|---|
| Workspace | `idleTimeoutMinutes` + optional `gryvia.io/idle-action: pause` | Have ([workspaces](workspaces.md)) |
| Inference service | `spec.scaleToZero` + LLM gateway wake | Have ([inference-serving](inference-serving.md)) |
| AI job | `spec.idleTimeout` → phase `Cancelled` | Have (this page) |

## Job `idleTimeout`

While the job is `Running`, the ai-operator compares now to `metadata.annotations["gryvia.io/last-activity"]`
(RFC3339) or, if unset, `status.startTime`. When the gap exceeds `spec.idleTimeout`, it finishes the job as
`Cancelled` with reason `IdleTimeout` and tears down the workload (PVC kept).

Patch activity from tools or the CLI:

```bash
kubectl annotate gryviaaijob my-job gryvia.io/last-activity="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

`gryvia workspace ssh` / `sync` already refresh the annotation on workspaces.
