# Agents

A `GryviaAgent` serves a tool-calling agent as an OpenAI-compatible endpoint. You give it a model published on the
[LLM gateway](llm-gateway.md), a system prompt and a list of tools. The ai-operator runs a runtime Deployment and a
Service for each agent. For every request, the runtime runs a loop: it calls the model with the tools' schemas, runs
the tools the model asks for, and feeds the results back, for at most `maxSteps` model calls. Tools can search a
[vector index](rag.md) through the gateway's `/v1/retrieve`, call allowlisted HTTP endpoints, or search a
[Zyntra](sovereign-aios.md) ontology and propose actions there for people to approve.

Each agent has its own gateway key, so its tokens are metered under its namespace and capped by the tenant's quota
like any other caller's. A NetworkPolicy limits what the runtime can reach.

The feature is opt-in and needs the LLM gateway.

## What is verified and what is not

| Verified | How |
| --- | --- |
| Deployment (non-root, read-only root filesystem, no service account token), Service, config ConfigMap, a pod roll on config changes, the agent's own gateway key (raw key in the agent's namespace, hash in the key namespace), NetworkPolicy rules for the gateway, in-cluster, IP and public tool hosts, Zyntra tools (config, token env var from the Secret, egress to Zyntra), validation, a missing vector index, scale to zero, finalizer clean-up that leaves a same-named index's store credential alone | Fake-client tests in `operators/ai-operator/controllers/gryviaagent_controller_test.go` |
| The tool-calling loop, summed usage, the last step without tools, retrieval and HTTP tools, Zyntra tools (search, object, propose against a fake Zyntra that checks the token; the action allowlist, refused proposals, a missing or wrong token), the URL allowlist (scheme, host, port, normalized path; no user info), tool errors handed back to the model, gateway errors, request validation; streaming: relayed content deltas, tool-call deltas assembled across chunks, tool-call events, errors before and after the first event, a gateway that answers without streaming | `examples/agents/test_runtime.py` (scripted gateway on `httpx.MockTransport`) |
| Routes (including the chat proxy, which ignores a `status.endpoint` outside the agent's namespace, and the streamed proxy with its error mapping), CLI and dashboard helpers | `services/api-gateway/tests/test_agents.py`, `cli/src/commands/agents.rs`, `web-ui/src/lib/agents.test.ts` |
| NetworkPolicy enforcement on kind (kindnet enforces NetworkPolicy since kind v0.24): the agent reaches its allowlisted tool host but not a server in another namespace that a pod there can reach, and that namespace cannot reach the agent | The "Agents" steps of `.github/workflows/e2e-ml.yml` |
| The whole path on kind: the real runtime image, a stand-in tool-calling model published on the gateway, chat through the api-gateway with an HTTP tool fetching a file (also streamed), a refused URL, metering, scale to zero and deletion | The "Agents" steps of `.github/workflows/e2e-ml.yml`; the non-streamed steps also passed on a single-node k3s host (2026-10-01) |
| A real model deciding to call a tool: Qwen2.5-0.5B Instruct on llama.cpp's server (CPU, greedy decoding) behind the gateway; the agent runtime offers an HTTP tool, the model calls it, the runtime fetches the handbook and the model answers from it, non-streamed and streamed (the tool-call deltas come from the real engine) | The "Real model" step of `.github/workflows/e2e-ml.yml`, passed in kind CI on main ([run 36990962579](https://github.com/zyvorai/gryvia/actions/runs/36990962579), 2026-10-02); the same runtime loop also passed 5 of 5 runs locally against llama.cpp b11146 |
| Zyntra tools in a cluster, with a real model: the chart's token Secret, egress through the NetworkPolicy, search and propose against Zyntra, the token refused approval; Qwen2.5-0.5B on llama.cpp choosing the Zyntra search tool and answering from the ontology | [`.github/workflows/e2e-sovereign-aios.yml`](../.github/workflows/e2e-sovereign-aios.yml), see [Sovereign AI OS](sovereign-aios.md#what-is-verified-and-what-is-not) |

| Not verified | Why |
| --- | --- |
| A tool-calling model served by vLLM, or larger models and multi-tool plans | No GPU in CI. The real-model run uses llama.cpp on CPU with a 0.5B model and one tool |
| NetworkPolicy enforcement with other CNIs (Calico, Cilium) | Only kind's kindnet was run; any CNI that implements NetworkPolicy should behave the same |
| Streaming through vLLM | Streaming tool-call deltas were run with llama.cpp only; vLLM's follow the same format |

## Turning it on

```yaml
llmGateway:
  enabled: true
aiOperator:
  agents:
    enabled: true
    image: ghcr.io/zyvorai/gryvia-agent-runtime:latest   # --agent-image (examples/agents)
```

The chart passes `--enable-agents`, `--agent-image`, `--llm-gateway-url` (the release's gateway Service) and
`--llm-key-namespace` (`llmGateway.keyNamespace`) to the ai-operator. Rendering fails if `aiOperator.agents.enabled`
is set without `llmGateway.enabled`. The operators ClusterRole covers what the controller creates (Deployments,
Services, ConfigMaps, Secrets and NetworkPolicies).

## An agent

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAgent
metadata:
  name: helper
  namespace: tenant-alpha
spec:
  model: chat                        # a model published on the LLM gateway that supports tool calling
  systemPrompt: |
    Search the handbook before answering and cite the source file.
  maxSteps: 5                        # model calls per request (1-20, default 5)
  replicas: 1                        # 0 scales the runtime down (default 1)
  tools:
    - name: search_handbook          # the function name the model sees
      description: Search the team handbook.
      type: retrieval
      retrieval:
        vectorIndexRef: handbook     # a GryviaVectorIndex in this namespace
        topK: 4                      # passages returned (1-20, default 4)
    - name: platform_status
      type: http
      http:
        urls: [https://status.example.com/api/]   # allowed URL prefixes
        method: GET                  # or POST (the model then passes a body)
  image: ""                          # default: --agent-image
  resources:
    requests: {cpu: 50m, memory: 128Mi}
```

[examples/agents/agent.yaml](../examples/agents/agent.yaml) is a complete example.

For vLLM, the model must be served with tool calling turned on (`--enable-auto-tool-choice` and the
`--tool-call-parser` for the model). The LLM gateway passes `tools` and `tool_choice` through unchanged.

### Zyntra tools

A `zyntra` tool gives the model a [Zyntra](https://github.com/zyvorai/zyvor-zyntra-site) ontology: the typed business objects,
links and KPIs Zyntra builds from its connectors (in the [Sovereign AI OS](sovereign-aios.md) release, Gryvia's
tenants, GPU nodes, jobs, models, inference services and datasets).

```yaml
  tools:
    - name: ops                      # the model sees ops_search, ops_object and ops_propose (at most 56 characters)
      description: the GPU platform's ontology
      type: zyntra
      zyntra:
        url: http://zyntra.gryvia-system.svc:8080
        tokenSecretRef: {name: zyntra-agent-token, key: token}   # a Secret in the agent's namespace
        propose: true                # offer ops_propose
        actions: [raise-inference-priority]   # optional allowlist; empty allows what the token may propose
```

| Function | Zyntra call |
| --- | --- |
| `<name>_search` (`query`, `type`) | `GET /api/v1/ontology/objects?q=&type=&limit=20`; one line per object with its properties |
| `<name>_object` (`id`) | `GET /api/v1/ontology/objects/{id}`; properties, links and failing KPIs as JSON |
| `<name>_propose` (`action`, `inputs`) | `POST /api/v1/proposals`. Zyntra checks the action's contract, constraints and preconditions; a refusal and its reasons go back to the model |

The token is a Zyntra service token, made with `zyntra service-token -name agent -roles viewer,proposer` and listed
under `service_tokens` in Zyntra's policy file. The [Sovereign AI OS](sovereign-aios.md#agents-on-the-ontology)
chart generates one and puts it in the agent namespaces you list. Zyntra gives service tokens the viewer and proposer roles only, so an
agent can draft a proposal but never approve it: approval stays with people in Zyntra, and typed actions marked
`permissions: [approver]` refuse proposals from the agent. A token bound to a tenant sees only that tenant's objects.
The controller puts the token in the runtime's environment (`ZYNTRA_TOKEN_<NAME>`), not in the config ConfigMap.

## What the controller creates

All objects are named `<agent>-agent`, are owned by the agent, and go with it.

| Object | Contents |
| --- | --- |
| Secret `<agent>-llm-key` | The agent's raw gateway key (data key `key`). Its hash is in `<keyNamespace>/<namespace>.agent-<agent>`, labelled `gryvia.io/llm-key-owner=agent` |
| ConfigMap | `config.json`: name, model, system prompt, max steps and tools, mounted at `/etc/agent` |
| Deployment | The runtime, with `GATEWAY_URL`, `GRYVIA_LLM_KEY` (from the Secret) and `AGENT_CONFIG`; uid 65534, read-only root filesystem with a writable `/tmp`, no service account token. A hash of the config in the pod template rolls the pods when the spec changes |
| Service | Port 8080 |
| NetworkPolicy | Below |

The agent is `Ready` once a replica is ready. Its status carries `endpoint`
(`http://<agent>-agent.<namespace>.svc.cluster.local:8080`), `readyReplicas` and `keySecret`. The `ToolsResolved`
condition is false while a retrieval tool names a vector index that does not exist; the agent still runs, and that
tool returns an error to the model. With `replicas: 0` the phase is `Pending` with "Scaled to zero".

### Network policy

Ingress is allowed on port 8080 from pods in the agent's namespace and from the api-gateway pods, which proxy the
dashboard's and CLI's chat.

Egress is allowed only to:

- DNS (port 53 in any namespace).
- The LLM gateway pods (namespace of `--llm-gateway-url`, label `app.kubernetes.io/component=llm-gateway`). Retrieval
  also goes through the gateway, so the runtime never talks to a vector store.
- The hosts of the HTTP tools' URLs and of the Zyntra tools' `url`:
  - an in-cluster service (`svc`, `svc.ns`, `svc.ns.svc...`): its namespace, any port, because Service and container
    ports can differ;
  - an IP literal: that address on the URL's port;
  - any other hostname: the URL's port on public addresses only. NetworkPolicy cannot match hostnames, and private
    ranges (10/8, 172.16/12, 192.168/16, 169.254/16, 100.64/10, fc00::/7, fe80::/10) are excluded so a public tool
    does not open the cluster network.

Zyntra tools open their `url` the same way.

NetworkPolicy needs a CNI that enforces it. Either way, the runtime only calls URLs that match an allowlisted prefix.

## Calling an agent

From inside the cluster (pods in the agent's namespace), the endpoint is a regular chat completion API:

```bash
curl http://helper-agent.tenant-alpha.svc.cluster.local:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"How are GPU hours capped?"}]}'
```

The `model` field of the request is ignored. The reply's `model` is the agent's name, `usage` sums every model call,
and an extra `gryvia` block lists the steps and tool calls:

```json
{"object": "chat.completion", "model": "helper",
 "choices": [{"index": 0, "message": {"role": "assistant", "content": "…"}, "finish_reason": "stop"}],
 "usage": {"prompt_tokens": 412, "completion_tokens": 60, "total_tokens": 472},
 "gryvia": {"steps": 2, "toolCalls": [{"step": 1, "tool": "search_handbook", "arguments": {"query": "GPU hours"}, "ok": true}]}}
```

`finish_reason` is `length` when the model still asked for tools on the last step. Gateway errors come back in
the OpenAI error shape: 401, 403, 404, 429 and 503 pass through, and anything else is a 502.

### Streaming

With `"stream": true` the reply is server-sent events in the OpenAI `chat.completion.chunk` format, ending with
`data: [DONE]`. The runtime streams every model call from the gateway and relays the text as it arrives; the
tool calls of a step are assembled from their deltas and run before the next model call. Besides the usual content
chunks there are two kinds of extra chunk, both with an empty `choices` list:

- one per tool call, as soon as it has run: `{"choices": [], "gryvia": {"toolCall": {"step": 1, "tool": "search_handbook", "arguments": {…}, "ok": true}}}`;
- a last one with the summed `usage` and `"gryvia": {"steps": …, "toolCalls": […]}`, after the chunk that carries
  `finish_reason`.

A gateway error before the first event is an HTTP error as above. Once events have been sent, an error becomes an
event `{"error": {"message": …, "type": "gateway_error", "code": 429}}` followed by `[DONE]`.

The runtime does not authenticate callers. Access is limited by the NetworkPolicy, so do not expose the Service
outside the cluster.

Through the platform API, with a dashboard or API key:

| Route | Behaviour |
| --- | --- |
| `GET /api/agents`, `GET /api/agents/{name}` | List and show agents of the caller's namespaces |
| `POST /api/agents` | Create one: `name`, `model`, `systemPrompt`, `tools[]` (`name`, `type`, `vectorIndexRef`/`topK` or `urls`/`method`), `maxSteps`, `replicas`, `image`, `cpu`, `memory` |
| `POST /api/agents/{name}/chat` | `{"messages": [{"role", "content"}], "stream": false}`; proxied to the runtime (409 unless the agent is `Ready`). With `"stream": true` the runtime's events are relayed as they arrive |
| `POST /api/agents/{name}/scale` | `{"replicas": n}` |
| `DELETE /api/agents/{name}` | Delete it |

```bash
gryvia agents create -f examples/agents/agent.yaml -n tenant-alpha
gryvia agents list -n tenant-alpha
gryvia agents chat helper how are GPU hours capped       # needs GRYVIA_GATEWAY_URL and GRYVIA_API_KEY
gryvia agents delete helper -n tenant-alpha --yes
```

The dashboard's **Agents** page (under Models) lists agents, scales them and has a chat panel.

## Keys and metering

The agent's key belongs to its namespace, so the tenant's `tokensPerDay` quota covers the agent's model calls and the
query embeddings of its retrieval tool. Usage records show the model name (`chat`, and the index's embedding model
for retrieval). The key is listed with the tenant's keys. The operator writes it back if it is revoked, so scale the
agent to zero or delete it instead.

## Your own runtime

Set `spec.image` (or `--agent-image`). The container must serve `GET /healthz` and `POST /v1/chat/completions` on
port 8080, read its configuration from the JSON file at `AGENT_CONFIG`, and call the gateway at `GATEWAY_URL` with
`GRYVIA_LLM_KEY`. See [examples/agents](../examples/agents/README.md).
