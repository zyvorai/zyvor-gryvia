# LLM gateway

One OpenAI-compatible endpoint in front of every `GryviaInferenceService` annotated `gryvia.io/llm-model`. Callers
use per-tenant API keys, the gateway routes on the request's `model`, meters tokens into `GryviaUsageRecord`s of kind
`tokens` and refuses requests with HTTP 429 once a `GryviaQuota.spec.tokensPerDay` is used up. It is opt-in.

It runs as `/manager llm-gateway`, a mode of the ai-operator binary (same image), in its own Deployment.

## What is verified and what is not

| Verified | How |
| --- | --- |
| Key authentication, routing by model (own namespace first, then shared), the model rewrite, the Authorization header not being forwarded, `/v1/models` scoped to the key, streaming with `stream_options.include_usage` and usage read from the last SSE chunk, OpenAI error bodies, 400/404/413/502 | Unit tests with `httptest` upstreams (`operators/ai-operator/pkg/llmgateway/llmgateway_test.go`) |
| Hourly token records (create, update while open, final after the hour), 429 | Same tests with a fake client |
| The shared daily counter: two replicas seeing each other's traffic after a sync, no double counting, seeding from today's records, 503 before the first read, the UTC day reset, deleting old counters, 30 concurrent syncs from three replicas losing no tokens | `TestQuotaIsSharedAcrossReplicasAndRejectsWith429` and `TestQuotaSyncsConcurrentReplicasWithoutLosingTokens` (fake client, race detector) |
| Scale to zero: a request for a `ScaledToZero` model sets `gryvia.io/wake-requested` once and is held until the service is ready, then proxied and metered; a `Deploying` service is waited on without a new wake; 503 with `Retry-After` after the cold-start timeout; no upstream call after the caller leaves; `gryvia.io/last-request` throttled per service; services without `scaleToZero` never annotated | `operators/ai-operator/pkg/llmgateway/activator_test.go` (race detector) |
| A real cold start on kind: a stand-in model with `idleSeconds: 60` gets `gryvia.io/last-request`, scales to zero (Deployment at 0, no pods), and the next request through the gateway wakes it, waits, gets 200 and is metered | The "LLM gateway - scale to zero and a cold start through the gateway" step of `.github/workflows/e2e-ml.yml` |
| Key create/list/revoke with tenant scoping, the key shown once and only its hash stored, models and usage routes | `services/api-gateway/tests/test_llm.py` |
| The Playground page: only ready models offered, the key required and checked, a streamed reply with its usage, the conversation resent on the next turn, an error kept out of the next request, curl and Python snippets without the key | `web-ui/src/pages/Playground.test.tsx`, `web-ui/src/lib/playground.test.ts` (vitest) |
| The playground route `POST /api/llm/chat`: forwarding with the caller's key as a bearer token, the key absent from logs and error bodies, streamed and plain replies, a rejected key as 400, 404/429 passed through, 502 when unreachable, validation | `services/api-gateway/tests/test_llm.py` |
| CLI key Secret, hashing, model and usage tables | `cli/src/commands/llm.rs` tests |
| The gateway on a real cluster against a stand-in OpenAI server: key, model list, a chat completion, a streamed one, the usage record and a 429; with two replicas, the shared counter and a 429 from the replica that served no traffic; the playground route through the dashboard's nginx (a chat, a streamed one with usage, a wrong key as 400, an unknown model as 404, both requests metered, a reply that takes 35 s) and the `/playground` page served | The "LLM gateway" step of `.github/workflows/e2e-ml.yml`; the single-replica steps also passed on a single-node k3s host (2026-10-01) |
| A real model engine behind the gateway: llama.cpp's server (CPU) with Qwen2.5-0.5B Instruct as a `GryviaInferenceService`, a chat completion with its real usage, a streamed answer with many content chunks and the usage chunk the gateway asks for, and the tokens metered under the model | The "Real model" step of `.github/workflows/e2e-ml.yml`; passed in kind CI on main ([run 36990962579](https://github.com/zyvorai/gryvia/actions/runs/36990962579), 2026-10-02: 22 streamed content chunks plus the usage chunk) |

| Not verified | Why |
| --- | --- |
| vLLM itself behind the gateway | No GPU in CI. A real engine was run instead: llama.cpp's OpenAI-compatible server on CPU (see the verified row above) |
| The counter under high request rates | One ConfigMap update per replica per second; not load-tested |

## Turning it on

```yaml
llmGateway:
  enabled: true
  replicas: 1
  keyNamespace: gryvia-llm-keys     # created by the chart; holds only the key Secrets
  priceInputPer1M: 0                # default prices per million tokens (annotations override them)
  priceOutputPer1M: 0
  currency: USD
  flushInterval: 5m                 # --flush-interval: how often usage records are written
  quotaRefreshInterval: 30s         # --quota-refresh-interval
  quotaSyncInterval: 1s             # --quota-sync-interval: how often replicas share their token counts
  service: {type: ClusterIP, port: 8080}
```

The chart adds the Deployment and Service `<release>-llm-gateway` in the release namespace, the namespace
`keyNamespace`, and:

| Who | Access |
| --- | --- |
| llm-gateway ServiceAccount (ClusterRole) | get/list/watch `gryviainferenceservices`, `gryviaquotas`, `gryviavectorindexes`; patch `gryviainferenceservices` (scale-to-zero annotations); get/list/create/update `gryviausagerecords` |
| llm-gateway ServiceAccount (Role in keyNamespace) | get/list/watch `secrets` |
| llm-gateway ServiceAccount (Role in the release namespace) | get/list/create/update/delete `configmaps` (the daily token counters) |
| api-gateway ServiceAccount (Role in keyNamespace) | get/list/create/delete `secrets` (key issue and revoke) |

Keep `keyNamespace` dedicated to keys: the api-gateway may create and delete any Secret in it. The namespace has
`helm.sh/resource-policy: keep`, so uninstalling the chart keeps the keys.

The api-gateway learns the key namespace and the gateway URL from `GRYVIA_LLM_KEY_NAMESPACE` and
`GRYVIA_LLM_GATEWAY_URL` (set by the chart).

## Publishing a model

Annotate the inference service. Its `status.endpoint` is the upstream; a service without an endpoint is not routed.

| Annotation | Meaning |
| --- | --- |
| `gryvia.io/llm-model` | The name callers put in `model` (letters, digits, `.`, `_`, `-`; at most 63) |
| `gryvia.io/llm-served-model` | What the upstream calls the model (vLLM `--served-model-name`), when different; the gateway rewrites `model` |
| `gryvia.io/llm-shared` | `"true"`: keys of every namespace may call it; otherwise only keys of the service's namespace |
| `gryvia.io/llm-price-input-per-1m`, `gryvia.io/llm-price-output-per-1m` | Price per million tokens for the usage records |

When two services publish the same model name, a key uses the one in its own namespace, else the first shared one
(by namespace name). See `examples/llm-gateway/published-model.yaml`.

## Keys

A key is `gk-` and random characters. Only its sha256 is stored, in a Secret of `keyNamespace` labelled
`gryvia.io/llm-key=true` with data `hash` and `namespace` and labels `gryvia.io/tenant`, `gryvia.io/llm-key-name`
and `gryvia.io/llm-key-namespace`. The key is shown once, at creation.

```bash
gryvia llm keys create ci -n tenant-alpha --description "CI runs"   # prints the key once
gryvia llm keys list -n tenant-alpha                                # or -A for every namespace
gryvia llm keys delete ci -n tenant-alpha --yes
```

The CLI writes the Secret directly, so it needs create rights on Secrets in `keyNamespace`. Tenant users create keys
through the dashboard ("LLM gateway" page) or `POST /api/llm-keys` instead; the api-gateway only lets them pick one
of their own namespaces. Revoking takes effect as soon as the gateway's informer sees the deletion (seconds).

Vector indexes ([RAG](rag.md)) get keys of their own from the ai-operator: Secrets `<namespace>.index-<name>` with the
label `gryvia.io/llm-key-owner=index`, listed with the tenant's keys. The operator writes them back on its next
reconcile, so suspend or delete the index rather than revoking its key. [Agents](agents.md) get keys the same way:
Secrets `<namespace>.agent-<name>`, label `gryvia.io/llm-key-owner=agent`. The gateway also caches the copies of external
vector-store API keys kept in `keyNamespace` (label `gryvia.io/llm-key=store`); they are not API keys.

## Calling it

```bash
curl http://<release>-llm-gateway.gryvia-system.svc.cluster.local:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"chat","messages":[{"role":"user","content":"Hello"}]}'
```

| Route | Behaviour |
| --- | --- |
| `GET /v1/models` | The models the key may call |
| `POST /v1/chat/completions`, `/v1/completions`, `/v1/embeddings` | Proxied to the model's endpoint; streaming responses are relayed event by event |
| `POST /v1/retrieve` | Nearest chunks of a `GryviaVectorIndex` of the key's namespace; the query embedding is metered (see [RAG](rag.md)) |
| `GET /healthz` | No key needed |

Errors use the OpenAI shape `{"error": {"message", "type", "code"}}`: 401 (missing or unknown key), 404 (unknown model
or not visible to the key), 413 (body over `--max-body-bytes`, 16 MiB), 429 (tokensPerDay used up), 502 (upstream
unreachable). The `Authorization` header is not forwarded upstream. For streamed chat and completion requests the
gateway sets `stream_options.include_usage` so the final chunk carries the token counts.

The Service is ClusterIP. Expose it with your own Ingress or Gateway and TLS if callers are outside the cluster.

## Playground (through the api-gateway)

The dashboard's **Playground** page (Models → Playground, `/playground`) chats with any ready published model: pick
the model, paste one of your keys (kept in the page's memory only; reloading forgets it), set a system prompt,
temperature, `max_tokens` and streaming, and see each reply's token usage. It shows the current settings as a curl
command or OpenAI Python client code against the gateway, with the key left as `$GRYVIA_LLM_KEY`.

The page calls `POST /api/llm/chat` on the api-gateway with the dashboard session and an LLM key in
the `X-LLM-Key` header. The api-gateway forwards the request to the gateway with that key as the bearer token, so the
key's namespace scoping, token quotas and metering apply as for any other caller. The key is not stored or logged.

```bash
curl -k https://<dashboard>/api/llm/chat -H "Authorization: Bearer $SESSION" -H "X-LLM-Key: $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"chat","messages":[{"role":"user","content":"Hello"}],"temperature":0.2,"max_tokens":64,"stream":true}'
```

The body takes `model`, `messages` (roles `system`, `user`, `assistant`), `temperature` (0 to 2), `max_tokens` and
`stream`; other fields are refused. With `stream: true` the gateway's events are relayed as they arrive. A key the
gateway rejects answers 400 (not 401, which would end the dashboard session); 404, 429 and 503 pass through; other
gateway errors are 502. The route is limited to 30 requests a minute per client. The dashboard's nginx gives the chat
routes (this one and agent chats) 300 s to answer and does not buffer their events; other `/api/` routes keep 30 s.

## Metering

Prometheus on port 8081 (`/metrics`):

| Metric | Labels |
| --- | --- |
| `gryvia_llm_tokens_total` | `tenant`, `model`, `type` (`input` or `output`) |
| `gryvia_llm_requests_total` | `tenant`, `model`, `code` |
| `gryvia_llm_quota_rejections_total` | `tenant` |

Every 5 minutes (`--flush-interval`) each replica writes one `GryviaUsageRecord` per tenant, model and hour in the
key's namespace: `kind: tokens`, `job: llm:<model>`, `gpus: 0`, `start`/`end` the hour, `inputTokens`,
`outputTokens`, `cost` (from the prices) and `final: true` once the hour is over. Records are labelled
`gryvia.io/usage-kind=tokens`. Budgets and chargeback that sum `cost` include them; GPU-hour totals do not change.
A replica that dies loses at most its last 5 minutes of counts.

```bash
gryvia llm usage -n tenant-alpha --group-by day --days 7
gryvia llm usage -A --group-by tenant
```

`GET /api/llm/usage?groupBy=model|tenant|namespace|day&from=&to=` returns the same sums plus each quota's tokens used
today.

## Quotas

Set `tokensPerDay` on a `GryviaQuota`; it applies to keys of the namespaces in `spec.namespaces`, counting input plus
output tokens per UTC day:

```yaml
spec:
  namespaces: [tenant-research]
  tokensPerDay: 2000000
```

The gateway reloads quotas every 30 seconds. Today's counts live in a ConfigMap shared by every replica,
`gryvia-llm-tokens-<YYYY-MM-DD>` in the gateway's namespace (label `gryvia.io/llm-token-counter=true`, data:
namespace to tokens). Every second (`--quota-sync-interval`) each replica adds the tokens it counted since its last
sync, with an optimistic-concurrency update that retries on conflict, and reads back the totals. A request is checked
against those totals plus this replica's tokens not yet synced:

- A replica sees another replica's traffic within about two sync intervals. Over the limit by more than one request
  therefore takes requests in flight or finished in the last couple of seconds on other replicas.
- A request is refused when the count is at or over the limit, so the last request may go over it by its own size.
- The first replica to create a day's ConfigMap seeds it from that day's token records, which covers traffic from
  before the counter existed (for example, an upgrade during the day).
- A restarted replica loses at most its last second of counts; the final sync on shutdown writes them.
- If the counter cannot be read, a replica keeps checking against its last totals plus its own traffic, and logs the
  error. Before the first successful read, keys under a `tokensPerDay` quota get 503.
- Counters older than yesterday are deleted when a new day's counter is created.

## Scale to zero

For a model whose service sets `spec.scaleToZero.enabled` ([inference serving](inference-serving.md#scale-to-zero)):

- After each request it proxies (any status below 500), the gateway writes `gryvia.io/last-request` on the service,
  at most once a minute per service and replica (every `idleSeconds / 3` when that is shorter), so a service in use
  is not scaled down.
- A request for a service in phase `ScaledToZero` sets `gryvia.io/wake-requested` (repeated at most every 5 seconds
  while requests wait) and is held, re-reading the services every second, until the phase is no longer
  `ScaledToZero` or `Deploying`. It is then proxied and metered as usual. Requests arriving while the service is
  `Deploying` wait the same way. A refused connection right after the wake-up (the Service's endpoints can trail
  the ready replica) is retried for up to 30 seconds; nothing reached the model, so nothing runs twice.
- If the service is not ready within `coldStartTimeoutSeconds` (default 300), the request gets 503 with
  `Retry-After: 30`. A caller that disconnects while waiting is dropped; the wake-up still goes ahead.

The quota check runs before the wait, so a key over its `tokensPerDay` limit does not wake a model. Keep client and
proxy timeouts above the cold-start time: the api-gateway playground allows 300 seconds.

## Flags

| Flag | Default |
| --- | --- |
| `--listen-address` | `:8080` |
| `--metrics-address` | `:8081` |
| `--key-namespace` | `gryvia-llm-keys` |
| `--price-input-per-1m`, `--price-output-per-1m` | `0` |
| `--currency` | `USD` |
| `--flush-interval` | `5m` |
| `--quota-refresh-interval` | `30s` |
| `--quota-sync-interval` | `1s` |
| `--counter-namespace` | `$POD_NAMESPACE` (the chart sets it) |
| `--max-body-bytes` | 16 MiB |
| `--upstream-timeout` | `10m` |
