# Agents: tool-calling endpoints on your models

A `GryviaAgent` is a model, a system prompt and a set of tools, served as an OpenAI-compatible
`POST /v1/chat/completions`. The ai-operator (chart value `aiOperator.agents.enabled`) runs one runtime Deployment
per agent. Each runtime calls its model through the LLM gateway with its own key, so its tokens are metered and
capped like any other caller's. A NetworkPolicy limits the runtime's egress to DNS, the gateway and the allowlisted
tool hosts.

> **Status.** The controller, `runtime.py` and the api-gateway chat proxy are unit-tested. The e2e workflow runs an
> agent on kind against a stand-in tool-calling model and an HTTP tool. Real tool-calling models served by vLLM
> have not been tried. See [docs/agents.md](../../docs/agents.md).

## Files

- [agent.yaml](agent.yaml): a handbook assistant with a retrieval tool and an HTTP tool
- [runtime.py](runtime.py): the reference runtime (FastAPI): the tool-calling loop, the retrieval, HTTP and Zyntra tools
- [Dockerfile](Dockerfile): the reference image `ghcr.io/zyvorai/gryvia-agent-runtime` (the operator's `--agent-image`)
- [test_runtime.py](test_runtime.py): tests against a scripted gateway (`httpx.MockTransport`)

## Run it

```bash
helm upgrade gryvia helm/gryvia --reuse-values \
  --set llmGateway.enabled=true --set aiOperator.agents.enabled=true

kubectl apply -f examples/agents/agent.yaml
gryvia agents list -n tenant-alpha
gryvia agents get helper -n tenant-alpha

export GRYVIA_GATEWAY_URL=https://gryvia.example.com GRYVIA_API_KEY=...
gryvia agents chat helper how are GPU hours capped
```

## How a request runs

1. The runtime prepends the system prompt and sends the conversation to the model with the tools' JSON schemas.
2. When the model returns tool calls, the runtime runs them and appends the results as `tool` messages:
   - **retrieval**: the gateway's `POST /v1/retrieve` on the index, with `topK` passages.
   - **http**: a GET (or POST with the model's `body`) of the URL the model gave. The URL must match an allowlisted
     prefix by scheme, host, port and path, after the path is normalized. Responses are capped at 64 KiB and
     truncated to 8000 characters for the model. Redirects are not followed.
   - **zyntra**: `<name>_search`, `<name>_object` and `<name>_propose` call Zyntra's ontology and proposal API with
     the service token from `ZYNTRA_TOKEN_<NAME>`. A proposal waits for people to approve it in Zyntra
     ([docs/agents.md](../../docs/agents.md#zyntra-tools)).
3. This repeats up to `maxSteps` model calls. The last call offers no tools, so the model has to answer.

The response is a regular chat completion with `model` set to the agent's name, the summed `usage` of every model
call, and a `gryvia` block: `{"steps": n, "toolCalls": [{"step", "tool", "arguments", "ok"}]}`. With
`"stream": true` the model calls are streamed and the reply is `chat.completion.chunk` events, with one extra event per
tool call and a last one carrying the usage and the `gryvia` block (see [docs/agents.md](../../docs/agents.md#streaming)).

## Your own runtime

Set `spec.image`. The container must serve `GET /healthz` and `POST /v1/chat/completions` on port 8080. It gets
`AGENT_CONFIG` (the path of a JSON file with `name`, `model`, `systemPrompt`, `maxSteps` and `tools`), `GATEWAY_URL`
and `GRYVIA_LLM_KEY`. The root filesystem is read-only; `/tmp` is writable.

Test the reference runtime:

```bash
pip install -r examples/agents/requirements.txt pytest
cd examples/agents && pytest -q
```
