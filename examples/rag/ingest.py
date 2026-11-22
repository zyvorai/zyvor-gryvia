# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Ingest a dataset into a Qdrant collection for a GryviaVectorIndex.

The GryviaVectorIndex controller runs this script as a Job with the dataset version mounted read-only at DATA_DIR.
It chunks every text file, embeds the chunks through the Gryvia LLM gateway (/v1/embeddings, metered under the
index's own key), upserts them into a fresh collection <COLLECTION>__<run>, then points the alias COLLECTION at it
and drops the previous collections. Queries (the gateway's /v1/retrieve) go to the alias, so they keep answering
from the previous ingestion until the switch.

Environment (set by the controller):
  GATEWAY_URL, GRYVIA_LLM_KEY      LLM gateway and the index's key
  EMBED_MODEL, EMBED_BATCH         embedding model published on the gateway, chunks per request
  STORE_URL, STORE_API_KEY         Qdrant base URL and optional API key
  COLLECTION, RUN_ID               alias queried by /v1/retrieve, unique id of this ingestion
  CHUNK_SIZE, CHUNK_OVERLAP        in characters
  DATA_DIR, DATASET_VERSION        mounted dataset version

Inputs: .jsonl files contribute one document per line (the "text", "content" or "document" field) and are read line
by line, so they may be of any size (a single line above 20 MiB is skipped); other files with a text extension are
one document each, up to 20 MiB (HTML tags are stripped). Binary files are skipped. Chunks are embedded and upserted
as they are produced, so memory use depends on the batch sizes and the largest document, not on the dataset size.

On success the termination message is {"documents", "chunks", "dimensions", "collection"}; on failure it is the
error, which the controller shows in the index status.
"""
import hashlib
import html
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
import uuid

TERMINATION_LOG = os.environ.get("GRYVIA_TERMINATION_LOG", "/dev/termination-log")
TEXT_EXTENSIONS = {
    ".txt", ".md", ".markdown", ".rst", ".adoc", ".html", ".htm", ".json", ".jsonl", ".csv", ".tsv", ".xml",
    ".yaml", ".yml", ".py", ".go", ".rs", ".js", ".ts", ".java", ".c", ".h", ".cpp", ".sh", ".sql", ".tex", ".org",
}
MAX_FILE_BYTES = 20 * 1024 * 1024
MAX_RECORD_BYTES = 20 * 1024 * 1024
POINT_NAMESPACE = uuid.UUID("6f1d2c3a-4b5e-4f60-8a71-9b8c7d6e5f40")
UPSERT_BATCH = 128
PROGRESS_EVERY = 10


class IngestError(Exception):
    pass


def env(name, default=None):
    v = os.environ.get(name, default)
    if v is None or v == "":
        raise IngestError(f"environment variable {name} is not set")
    return v


# --- reading -----------------------------------------------------------------------------------------------------

_TAG = re.compile(r"<(script|style)\b.*?</\1>|<[^>]+>", re.S | re.I)


def strip_html(text):
    return html.unescape(_TAG.sub(" ", text))


def jsonl_lines(f, rel):
    """Yields (line number, bytes) of a binary file object, one line at a time. A line longer than
    MAX_RECORD_BYTES is skipped without holding it in memory."""
    n = 0
    while True:
        line = f.readline(MAX_RECORD_BYTES + 1)
        if not line:
            return
        n += 1
        if len(line) > MAX_RECORD_BYTES and not line.endswith(b"\n"):
            while line and not line.endswith(b"\n"):
                line = f.readline(MAX_RECORD_BYTES + 1)
            print(f"skip {rel}#{n}: line longer than {MAX_RECORD_BYTES} bytes", flush=True)
            continue
        yield n, line


def jsonl_documents(f, rel):
    for n, raw in jsonl_lines(f, rel):
        line = raw.decode("utf-8", errors="replace").strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        if isinstance(obj, dict):
            body = next((obj[k] for k in ("text", "content", "document") if isinstance(obj.get(k), str)), "")
            if body.strip():
                yield f"{rel}#{n}", body


def read_documents(root):
    """Yields (source, text) for every document under root, in a stable order."""
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(d for d in dirnames if not d.startswith("."))
        for name in sorted(filenames):
            if name.startswith("."):
                continue
            path = os.path.join(dirpath, name)
            rel = os.path.relpath(path, root)
            ext = os.path.splitext(name)[1].lower()
            if ext not in TEXT_EXTENSIONS:
                continue
            try:
                if ext == ".jsonl":
                    with open(path, "rb") as f:
                        if b"\x00" in f.read(8192):
                            continue
                        f.seek(0)
                        yield from jsonl_documents(f, rel)
                    continue
                if os.path.getsize(path) > MAX_FILE_BYTES:
                    print(f"skip {rel}: larger than {MAX_FILE_BYTES} bytes", flush=True)
                    continue
                with open(path, "rb") as f:
                    raw = f.read()
            except OSError as e:
                print(f"skip {rel}: {e}", flush=True)
                continue
            if b"\x00" in raw[:8192]:
                continue
            text = raw.decode("utf-8", errors="replace")
            if ext in (".html", ".htm"):
                text = strip_html(text)
            if text.strip():
                yield rel, text


def chunk(text, size, overlap):
    """Splits text into chunks of at most size characters overlapping by overlap. A chunk ends at the last
    paragraph break in the second half of its window, else the last line break, else the last space."""
    text = re.sub(r"[ \t]+", " ", text).strip()
    if not text:
        return []
    if len(text) <= size:
        return [text]
    out, start = [], 0
    while start < len(text):
        end = min(start + size, len(text))
        if end < len(text):
            lo = start + size // 2
            for sep in ("\n\n", "\n", " "):
                cut = text.rfind(sep, lo, end)
                if cut > start:
                    end = cut
                    break
        piece = text[start:end].strip()
        if piece:
            out.append(piece)
        if end >= len(text):
            break
        start = max(end - overlap, start + 1)
    return out


# --- HTTP --------------------------------------------------------------------------------------------------------


def request(method, url, body=None, headers=None, timeout=120, retries=4):
    data = None if body is None else json.dumps(body).encode()
    hdrs = {"Content-Type": "application/json", **(headers or {})}
    for attempt in range(retries + 1):
        req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                raw = resp.read()
                return resp.status, (json.loads(raw) if raw else {})
        except urllib.error.HTTPError as e:
            with e:
                raw = e.read()
            try:
                payload = json.loads(raw) if raw else {}
            except ValueError:
                payload = {"error": raw.decode(errors="replace")[:500]}
            if (e.code == 429 or e.code >= 500) and attempt < retries:
                time.sleep(min(2 ** attempt, 30))
                continue
            return e.code, payload
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            if attempt < retries:
                time.sleep(min(2 ** attempt, 30))
                continue
            raise IngestError(f"{method} {url}: {e}")
    raise IngestError(f"{method} {url}: retries exhausted")


def error_text(payload):
    err = payload.get("error") if isinstance(payload, dict) else None
    if isinstance(err, dict):
        return err.get("message") or json.dumps(err)
    if err:
        return str(err)
    if isinstance(payload, dict) and payload.get("status"):
        return json.dumps(payload["status"])
    return json.dumps(payload)[:300]


class Embedder:
    def __init__(self, gateway, key, model):
        self.url = gateway.rstrip("/") + "/v1/embeddings"
        self.headers = {"Authorization": f"Bearer {key}"}
        self.model = model
        self.tokens = 0

    def embed(self, texts):
        code, payload = request("POST", self.url, {"model": self.model, "input": texts}, self.headers)
        if code != 200:
            raise IngestError(f"embedding with model {self.model} failed ({code}): {error_text(payload)}")
        data = sorted(payload.get("data") or [], key=lambda d: d.get("index", 0))
        if len(data) != len(texts):
            raise IngestError(f"embedding returned {len(data)} vectors for {len(texts)} inputs")
        self.tokens += int((payload.get("usage") or {}).get("total_tokens") or 0)
        return [d["embedding"] for d in data]


class Qdrant:
    def __init__(self, url, api_key=None):
        self.url = url.rstrip("/")
        self.headers = {"api-key": api_key} if api_key else {}

    def call(self, method, path, body=None, ok=(200,)):
        code, payload = request(method, self.url + path, body, self.headers)
        if code not in ok:
            raise IngestError(f"vector store {method} {path} failed ({code}): {error_text(payload)}")
        return code, payload

    def collections(self):
        _, p = self.call("GET", "/collections")
        return [c["name"] for c in (p.get("result") or {}).get("collections", [])]

    def aliases(self):
        _, p = self.call("GET", "/aliases")
        return {a["alias_name"]: a["collection_name"] for a in (p.get("result") or {}).get("aliases", [])}

    def create(self, name, dim):
        self.call("PUT", f"/collections/{name}", {"vectors": {"size": dim, "distance": "Cosine"}})

    def delete(self, name):
        self.call("DELETE", f"/collections/{name}", ok=(200, 404))

    def upsert(self, name, points):
        self.call("PUT", f"/collections/{name}/points?wait=true", {"points": points})

    def point_alias(self, alias, collection, current):
        actions = []
        if current is not None:
            actions.append({"delete_alias": {"alias_name": alias}})
        actions.append({"create_alias": {"collection_name": collection, "alias_name": alias}})
        self.call("POST", "/collections/aliases", {"actions": actions})


# --- ingestion ---------------------------------------------------------------------------------------------------


def run_suffix(run_id):
    return hashlib.sha256(run_id.encode()).hexdigest()[:12]


def batched(items, size):
    batch = []
    for item in items:
        batch.append(item)
        if len(batch) >= size:
            yield batch
            batch = []
    if batch:
        yield batch


def ingest(cfg, embedder, store, log=print):
    collection = cfg["collection"]
    target = f"{collection}__{run_suffix(cfg['run_id'])}"
    counts = {"documents": 0, "chunks": 0}

    def chunks():
        for source, text in read_documents(cfg["data_dir"]):
            counts["documents"] += 1
            for i, piece in enumerate(chunk(text, cfg["chunk_size"], cfg["chunk_overlap"])):
                yield source, i, piece

    log(f"embedding with {embedder.model} into {target}", flush=True)
    dim, created, points = 0, False, []
    for n, batch in enumerate(batched(chunks(), cfg["batch"]), 1):
        vectors = embedder.embed([c[2] for c in batch])
        if not created:
            dim = len(vectors[0])
            if dim == 0:
                raise IngestError(f"model {embedder.model} returned empty embeddings")
            store.delete(target)
            store.create(target, dim)
            created = True
        for (source, i, piece), vec in zip(batch, vectors):
            if len(vec) != dim:
                raise IngestError(f"embedding dimension changed from {dim} to {len(vec)}")
            points.append({
                "id": str(uuid.uuid5(POINT_NAMESPACE, f"{source}\x00{i}")),
                "vector": vec,
                "payload": {"text": piece, "source": source, "chunk": i, "datasetVersion": cfg["dataset_version"]},
            })
            if len(points) >= UPSERT_BATCH:
                store.upsert(target, points)
                points = []
        counts["chunks"] += len(batch)
        if n % PROGRESS_EVERY == 0:
            log(f"embedded {counts['chunks']} chunks of {counts['documents']} documents", flush=True)
    if points:
        store.upsert(target, points)
    if not counts["chunks"]:
        raise IngestError(f"no text documents under {cfg['data_dir']} (dataset version {cfg['dataset_version']})")
    log(f"{counts['documents']} documents, {counts['chunks']} chunks", flush=True)

    aliases = store.aliases()
    current = aliases.get(collection)
    if current is None and collection in store.collections():
        # A plain collection with the alias's name (made by hand) would block the alias.
        store.delete(collection)
    store.point_alias(collection, target, current)
    for name in store.collections():
        if name.startswith(collection + "__") and name != target:
            store.delete(name)
    log(f"alias {collection} -> {target}; {embedder.tokens} embedding tokens", flush=True)
    return {"documents": counts["documents"], "chunks": counts["chunks"], "dimensions": dim, "collection": collection}


def config():
    cfg = {
        "collection": env("COLLECTION"),
        "run_id": env("RUN_ID"),
        "data_dir": env("DATA_DIR", "/data"),
        "dataset_version": os.environ.get("DATASET_VERSION", ""),
        "chunk_size": int(env("CHUNK_SIZE", "1000")),
        "chunk_overlap": int(env("CHUNK_OVERLAP", "200")),
        "batch": max(1, int(env("EMBED_BATCH", "32"))),
    }
    if cfg["chunk_overlap"] >= cfg["chunk_size"]:
        raise IngestError("CHUNK_OVERLAP must be less than CHUNK_SIZE")
    return cfg


def write_termination(text):
    try:
        with open(TERMINATION_LOG, "w") as f:
            f.write(text[:4000])
    except OSError:
        pass


def main():
    try:
        cfg = config()
        embedder = Embedder(env("GATEWAY_URL"), env("GRYVIA_LLM_KEY"), env("EMBED_MODEL"))
        store = Qdrant(env("STORE_URL"), os.environ.get("STORE_API_KEY") or None)
        out = ingest(cfg, embedder, store)
    except IngestError as e:
        print(f"error: {e}", file=sys.stderr, flush=True)
        write_termination(str(e))
        return 1
    data = json.dumps(out, sort_keys=True)
    write_termination(data)
    print("outputs:", data, flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
