# DRA ResourceClaims and LLM intent routing

Wave 3 competitor gaps: real DRA attachment for fractional GPUs, and lean Kinference-style intent routing.

## DRA (`gpu-isolation=dra`)

When a job carries `gryvia.io/gpu-share` and `gryvia.io/gpu-isolation: dra`, the AI operator’s
`PlanGPURuntime` / `ApplyRuntimePlan` now:

1. Sets `gryvia.io/dra-milli-gpu` (and optional memory) annotations (as before).
2. Attaches a Pod `resourceClaims` entry named `gryvia-gpu` referencing a
   `ResourceClaimTemplate` named `gryvia-gpu` (override with `gryvia.io/dra-claim-template`).
3. Adds a matching `resources.claims` entry on the trainer container.

**Status:** unit-level wiring only. A cluster must install an NVIDIA (or other) DRA driver and create
the `ResourceClaimTemplate`. Not verified on GPUs. Kind e2e without a DRA driver will leave pods
Pending if claims cannot bind—prefer `mps`/`mig` in demos.

```bash
gryvia submit --image … --gpu 0.5 --isolation dra -- …
```

## LLM intent routing + burst

The LLM gateway accepts an `IntentPolicy` (JSON) on the Gateway struct:

```json
{
  "simpleModel": "llama-1b",
  "largeModel": "llama-70b",
  "maxSimpleChars": 256,
  "burstURL": "https://api.openai.com",
  "burstModels": ["llama-70b"]
}
```

- Short user prompts (≤ `maxSimpleChars`) rewrite `model` to `simpleModel`.
- When the local route is scaled-to-zero / deploying and `burstURL` is set, the gateway proxies to
  the external base instead of waiting for cold start (`X-Gryvia-Burst: 1`).
- Responses include `X-Gryvia-Resolved-Model`.

Load policy via operator flag / ConfigMap in a follow-up; the library and unit tests are in
`pkg/llmgateway/intent.go`. Metering still records the tenant usage against the key.
