# Job submission preflight

The gateway can check a `GryviaAIJob` against the real Kubernetes admission chain before anything is
created. The preview is always available. Opt in to make every create pass the same check first.

## Preview

`POST /api/jobs/preflight` takes the same body as `POST /api/jobs`. The gateway:

1. checks the input: at most 512 KiB of JSON; only `apiVersion`, `kind`, `metadata` and `spec`;
   `metadata.name` a DNS label; `metadata` limited to name, namespace and string labels and
   annotations (no server metadata, owner references or finalizers); a non-empty `spec.image`;
   an integer `spec.gpus` from 1. The `gpuCount` alias is rejected because it is not a CRD field.
2. sets the namespace to the caller's first namespace, as job creation does.
3. creates the job with `dryRun=All` and `fieldValidation=Strict`, so the CRD schema, unknown
   fields and every admission webhook are checked and nothing is stored.

A successful report:

```json
{"admitted": true, "persisted": false, "namespace": "tenant-one", "name": "train",
 "source": "kubernetes-dry-run", "checks": ["gateway-input", "crd-schema", "kubernetes-admission"],
 "enforcedOnCreate": false,
 "warnings": ["no node currently carries GPU type \"H100\" (label gryvia.io/gpu)"],
 "limitations": ["No GPU capacity is reserved", "..."]}
```

`warnings` are the `Warning` headers of the dry run. Gryvia's job webhook uses them for its feasibility
findings against the current nodes: more GPUs per node than the largest node, a GPU type no node carries,
and the [preflight](workload-intelligence.md) summary when no pool fits (`Blocked`) or memory is unknown
(`Incomplete`). They do not block; `aiOperator.preflightEnforce` decides that after creation.

A rejection keeps the Kubernetes status code (400, 403, 404, 409, 422 or 429; anything else is 503)
and names the Kubernetes reason, the failing field paths with their reason codes, and unknown fields,
for example `Job admission preflight failed (Invalid; spec.gpus: FieldValueInvalid); no job was created`.
Kubernetes messages are never returned because they can echo values such as environment variables.

The Submit Job page has a **Check admission** button. Editing the form hides an earlier successful
report. Python: `await client.jobs.preflight(manifest_or_yaml_path)`.

## Enforcing it on create

```yaml
apiGateway:
  submissionAdmission:
    enforce: true   # default false
```

With `enforce`, every `POST /api/jobs` runs the input checks and the dry run first, and fails closed:
a rejected manifest, a webhook that does not support dry run, or an unreachable API server blocks the
create. The create itself also uses strict field validation. Mutations from the dry run are not
copied into the submitted job; the real create runs admission again. No RBAC is added, because a
dry-run create needs the same `create` permission the gateway already has.

Without `enforce`, `POST /api/jobs` behaves as before: the same light checks, unknown fields are
pruned by the API server, and `gpuCount` is still accepted by the gateway. Turning it on can reject
clients that relied on either.

## Limits

- This is schema and admission checking. It reserves no GPUs and does not certify model memory,
  quota or scheduling, which the controllers decide after creation. Webhook warnings reflect the
  nodes at that moment. The supplied-snapshot preflight in [workload intelligence](workload-intelligence.md)
  stays advisory.
- Admission webhooks must declare `sideEffects: None` or `NoneOnDryRun`. Gryvia's own webhooks do.
- A create can still fail after a successful preview if something changes in between. A transport
  error during the real create has an uncertain outcome; list the job before retrying.
- Jobs created directly through the Kubernetes API do not pass through the gateway.
- Tested with gateway, SDK and dashboard unit tests and a Helm render test. Not run against a live
  cluster's admission chain in CI.
