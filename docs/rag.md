# RAG: vector indexes and retrieval

A `GryviaVectorIndex` makes a `GryviaDataset` searchable. The ai-operator keeps a vector store for the index (a
managed Qdrant, or an external Qdrant you name). For each dataset version it runs an ingestion Job, which chunks the
files, embeds the chunks through the [LLM gateway](llm-gateway.md) and upserts them. Applications then call the
gateway's `POST /v1/retrieve` with their own API key and get the nearest chunks back. Both the embedding tokens of
ingestion and those of each query are metered like any other gateway traffic.

The feature is opt-in. It needs the LLM gateway (for embeddings, keys and metering) and the dataset controller (for
the data).

## What is verified and what is not

| Verified | How |
| --- | --- |
| Managed store (Service, non-root StatefulSet, PVC), external store with a mirrored API key, the index's own gateway key (raw key in the index's namespace, hash in the key namespace), ingestion Jobs per dataset version, spec change, reingest annotation and schedule tick, Ready/Failed/suspended states, validation, finalizer clean-up | Fake-client tests in `operators/ai-operator/controllers/gryviavectorindex_controller_test.go` |
| `/v1/retrieve`: namespace scoping, 404/409/400/429, query embedding through the published model, metering, Qdrant search request and reply mapping, store API key | `operators/ai-operator/pkg/llmgateway/retrieve_test.go` (`httptest` backends) |
| Reading text, JSONL and HTML, skipping binaries, chunking, batching, retries, the new-collection-then-alias switch, removal of old collections, the termination message | `examples/rag/test_ingest.py` (in-process fake gateway and Qdrant) |
| Routes, CLI and dashboard helpers | `services/api-gateway/tests/test_vector_indexes.py`, `cli/src/commands/rag.rs`, `web-ui/src/lib/rag.test.ts` |
| The whole path on kind: the real Qdrant image (`-unprivileged`, uid 1000) and the real ingestion image, a dataset from a stand-in file server, embeddings from a stand-in OpenAI server published on the gateway, Ready with the expected counts, the right nearest chunk, metering, reingest and deletion | The "RAG" steps of `.github/workflows/e2e-ml.yml`; these steps also passed on a single-node k3s host (2026-10-01) |

| Not verified | Why |
| --- | --- |
| A real embedding model served by vLLM | No GPU in CI; the stand-in returns OpenAI-shaped embeddings |
| External stores other than a Qdrant API (Qdrant Cloud has not been tried either) | Only Qdrant's REST API is implemented |
| Large datasets against a real store and model | Chunks are embedded and upserted as they are read (below), and a 69 MiB, 300,000-record JSONL corpus peaked at 0.2 MiB of Python allocations with stub embedder and store (339.5 MiB before the change); throughput and Qdrant behaviour at that size have not been measured |

## Turning it on

```yaml
llmGateway:
  enabled: true
storageOperator:
  datasets:
    enabled: true
aiOperator:
  rag:
    enabled: true
    qdrantImage: qdrant/qdrant:v1.12.6-unprivileged   # --rag-qdrant-image
    ingestImage: ghcr.io/zyvorai/gryvia-rag-ingest:latest   # --rag-ingest-image (examples/rag)
```

The chart passes `--enable-rag`, the two images, `--llm-gateway-url` (the release's gateway Service) and
`--llm-key-namespace` (`llmGateway.keyNamespace`) to the ai-operator. Rendering fails if `aiOperator.rag.enabled` is
set without `llmGateway.enabled`. The operators ClusterRole already covers what the controller creates (Services,
StatefulSets, Jobs, Secrets). The LLM gateway's ClusterRole gains read access to `gryviavectorindexes`.

## An index

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaVectorIndex
metadata:
  name: handbook
  namespace: tenant-alpha          # must be the dataset's spec.namespace (where its PVC is)
spec:
  datasetRef: handbook             # a GryviaDataset
  embedding:
    model: embed                   # a model published on the LLM gateway (gryvia.io/llm-model)
    batchSize: 32                  # chunks per embeddings request (default 32)
  chunking:                        # without a chunking block: 1000 characters, overlap 200
    size: 800
    overlap: 100
  store:
    type: managed                  # or external, with external.url and optional external.apiKeySecretRef
    managed:
      storageSize: 5Gi             # default 10Gi; storageClass and image are optional
  collection: handbook             # default: the index name
  schedule: "0 3 * * *"            # optional cron: rebuild even without a new dataset version
  suspend: false                   # true: no new ingestion; queries keep using the last one
  ingestImage: ""                  # optional: your own ingestion image
```

See `examples/rag/vector-index.yaml` for a complete set (dataset, published embedding model, managed and external
index).

### What the controller does

1. **Store.**
   - **Managed:** the Service and StatefulSet `<index>-qdrant`, on port 6333 with `/readyz` and `/livez` probes and a
     volume claim `storage`. The pod runs as uid 1000 with `fsGroup` 1000, as the `-unprivileged` Qdrant images expect.
     The status URL is `http://<index>-qdrant.<namespace>.svc.cluster.local:6333`.
   - **External:** the API key named by `apiKeySecretRef` (default key `token`) is copied to the Secret
     `<namespace>.<index>.vectorstore` in the gateway's key namespace, because `/v1/retrieve` needs it there.
2. **Key.** The index gets its own gateway key.
   - The raw key is kept in the Secret `<index>-llm-key` in the index's namespace, owned by the index.
   - Its hash is in `<namespace>.index-<index>` in the key namespace, labelled like any other key, so `gryvia llm keys
     list` shows it.
   - Ingestion tokens are therefore attributed to the index's namespace and count against its `tokensPerDay`.
   - Revoking that key only works until the next reconcile, which re-creates the hash Secret. Suspend or delete the
     index instead.
3. **Ingestion.**
   - When an ingestion runs: once the dataset is `ready`, a Job `<index>-ingest-<hash>` runs for each new dataset
     version, spec change (generation), new value of the annotation `gryvia.io/reingest`, or due schedule tick.
   - How the Job runs: it mounts the dataset's PVC read-only at the version's subPath as `/data`, runs as uid 65534,
     uses `backoffLimit` 1 and is deleted 24 hours after it finishes.
   - On success, `status.documents`, `chunks`, `dimensions`, `datasetVersion` and `lastIngested` come from the Job's
     termination message, and the phase is `Ready`.
   - On failure, the phase is `Failed` with the Job's message. Set a new `gryvia.io/reingest` value (or run
     `gryvia rag reingest`) to retry.
4. **Deletion.** A finalizer removes the hashed key and the mirrored store key. Owner references remove the Qdrant,
   its Service, the Jobs and the raw key Secret. The Qdrant volume claim follows the StatefulSet's retention policy
   (by default it stays until you delete it).

Phases: `Pending` (dataset not ready, or suspended before the first ingestion), `Provisioning` (store starting),
`Ingesting`, `Ready`, `Failed`. An index that has been ingested once keeps answering queries during later
ingestions and failures.

### The ingestion image

`examples/rag/ingest.py` uses only the Python standard library (`python:3.12-alpine`). It:

1. Reads every file with a text extension under `/data`, one document at a time.
   - `.jsonl` files contribute one document per line, from the `text`, `content` or `document` field. They are read
     line by line and may be of any size; a single line over 20 MiB is skipped.
   - HTML tags are stripped.
   - Binary files and other files over 20 MiB are skipped.
2. Splits each document into chunks of at most `size` characters overlapping by `overlap`. A chunk ends at the last
   paragraph break in the second half of its window, else at the last line break, else at the last space.
3. Embeds the chunks in batches (`embedding.batchSize`) through `POST /v1/embeddings` of the gateway, with the
   index's key, as they are produced: memory depends on the batch sizes and the largest document, not on the
   dataset. It retries on 429 and 5xx.
4. Creates a fresh collection `<collection>__<run>` (cosine distance, the model's dimension) when the first batch is
   embedded and upserts the points 128 at a time.
   Point ids are uuid5 of source and chunk number; the payload is `text`, `source`, `chunk` and `datasetVersion`.
5. Points the alias `<collection>` at the new collection and deletes the other `<collection>__*` collections.
   Queries go to the alias, so they switch atomically.
6. Writes `{"documents", "chunks", "dimensions", "collection"}` (or the error) to `/dev/termination-log`.

To use your own image, set `spec.ingestImage`. It is run as `python3 -u /app/ingest.py` with the environment
described at the top of the script.

## Querying

```bash
curl http://<release>-llm-gateway.<namespace>:8080/v1/retrieve \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"index": "handbook", "query": "How are GPU hours capped?", "topK": 4}'
```

```json
{
  "object": "list", "index": "handbook", "model": "embed",
  "data": [{"score": 0.83, "text": "Quotas cap the GPU hours of each tenant team…", "source": "handbook.md", "chunk": 0}],
  "usage": {"prompt_tokens": 7, "total_tokens": 7}
}
```

- The key must belong to the index's namespace; an index in another namespace is a 404. `topK` defaults to 4 and is
  capped at 50. The query is at most 8192 characters.
- The query is embedded with the index's `embedding.model`, which must be visible to the key: published in the same
  namespace, or shared. The tokens are metered under the key, and `tokensPerDay` applies (429).
- Errors:

  | Code | When |
  | --- | --- |
  | 409 | The index has not finished its first ingestion |
  | 502 | The embedding server or the store fails |

- Pass the returned `text` fields to a chat model as context: `/v1/chat/completions` through the same gateway.

## Gateway routes, CLI and dashboard

| Surface | What |
| --- | --- |
| `GET/POST /api/vector-indexes`, `GET/DELETE /api/vector-indexes/{name}`, `POST .../{name}/reingest`, `.../suspend`, `.../resume` | Manage indexes; the list also returns `retrieveURL` |
| `gryvia rag index list\|get\|create -f\|delete` | The same through the Kubernetes API |
| `gryvia rag reingest <name>` | Sets `gryvia.io/reingest` |
| `gryvia rag query <index> <text> [--top-k N]` | Calls `/v1/retrieve` (`GRYVIA_LLM_GATEWAY_URL`, `GRYVIA_LLM_KEY`); needs no kubeconfig |
| Dashboard: Models → Vector indexes | Table, details, re-ingest, suspend/resume, delete, a curl example |

Tenant roles (`gryvia-tenant-viewer`, `-member`, `-admin`) can read `gryviavectorindexes`. Members and admins can
also create, update and delete them.
