# RAG: vector indexes over datasets

A `GryviaVectorIndex` turns a `GryviaDataset` into a searchable collection. The ai-operator (chart value
`aiOperator.rag.enabled`) keeps a managed Qdrant for the index (or uses an external one) and runs an ingestion Job
for each dataset version, spec change, schedule tick or re-ingest request. The Job chunks the files, embeds the
chunks through the LLM gateway and upserts them. Applications then call the gateway's `POST /v1/retrieve`.

> **Status.** The controller, the gateway's `/v1/retrieve` and `ingest.py` are unit-tested. The e2e workflow runs the
> whole path on kind with the real Qdrant image and a stand-in embeddings server. Real embedding models served by
> vLLM have not been tried. See [docs/rag.md](../../docs/rag.md).

## Files

- [vector-index.yaml](vector-index.yaml): a dataset, a published embedding model, a managed index and an external one
- [ingest.py](ingest.py): the ingestion Job (Python standard library only): read, chunk, embed, upsert, switch the alias
- [Dockerfile](Dockerfile): the reference image `ghcr.io/zyvorai/gryvia-rag-ingest` (the operator's `--rag-ingest-image`)
- [test_ingest.py](test_ingest.py): tests against an in-process fake gateway and Qdrant

## Run it

```bash
helm upgrade gryvia helm/gryvia --reuse-values \
  --set llmGateway.enabled=true --set aiOperator.rag.enabled=true --set storageOperator.datasets.enabled=true

kubectl apply -f examples/rag/vector-index.yaml
gryvia rag index list -n tenant-alpha
gryvia rag index get handbook -n tenant-alpha

gryvia llm keys create app -n tenant-alpha        # the key of the application that queries
kubectl port-forward -n gryvia-system svc/gryvia-llm-gateway 8080:8080 &
export GRYVIA_LLM_GATEWAY_URL=http://localhost:8080 GRYVIA_LLM_KEY=gk-...
gryvia rag query handbook "How are GPU hours capped?" --top-k 3

gryvia rag reingest handbook -n tenant-alpha       # rebuild now
```

## Your own ingestion image

Set `spec.ingestImage`. The Job runs `python3 -u /app/ingest.py` with the environment listed at the top of
`ingest.py`, and the dataset version mounted read-only at `/data`. On success, write
`{"documents": n, "chunks": n, "dimensions": n}` to `/dev/termination-log`. On failure, write the error there.

Test the script:

```bash
python3 examples/rag/test_ingest.py
```
