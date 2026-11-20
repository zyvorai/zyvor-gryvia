# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Tests of ingest.py against an in-process fake LLM gateway and Qdrant. Run: python3 examples/rag/test_ingest.py"""
import hashlib
import http.server
import json
import os
import sys
import tempfile
import threading
import unittest
from urllib.parse import urlparse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ingest  # noqa: E402

DIM = 8


def fake_vector(text):
    h = hashlib.sha256(text.encode()).digest()
    return [b / 255 for b in h[:DIM]]


class Fake:
    """State of the fake gateway and Qdrant."""

    def __init__(self):
        self.collections = {}  # name -> {"dim": n, "points": {id: point}}
        self.aliases = {}
        self.embed_calls = []
        self.fail_embed = None
        self.fail_once = 0
        self.alias_actions = []


def handler(fake):
    class H(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def reply(self, code, body):
            data = json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def body(self):
            n = int(self.headers.get("Content-Length") or 0)
            return json.loads(self.rfile.read(n)) if n else {}

        def do_POST(self):
            path = urlparse(self.path).path
            b = self.body()
            if path == "/v1/embeddings":
                if self.headers.get("Authorization") != "Bearer gk-test":
                    return self.reply(401, {"error": {"message": "invalid API key"}})
                if fake.fail_once:
                    fake.fail_once -= 1
                    return self.reply(503, {"error": {"message": "busy"}})
                if fake.fail_embed:
                    return self.reply(404, {"error": {"message": fake.fail_embed}})
                fake.embed_calls.append(b)
                data = [{"index": i, "embedding": fake_vector(t)} for i, t in enumerate(b["input"])]
                data.reverse()  # the client must order by index
                return self.reply(200, {"data": data, "usage": {"total_tokens": len(b["input"]) * 3}})
            if path == "/collections/aliases":
                fake.alias_actions.append(b["actions"])
                for a in b["actions"]:
                    if "delete_alias" in a:
                        name = a["delete_alias"]["alias_name"]
                        if name not in fake.aliases:
                            return self.reply(404, {"status": {"error": f"alias {name} does not exist"}})
                        del fake.aliases[name]
                    if "create_alias" in a:
                        ca = a["create_alias"]
                        if ca["alias_name"] in fake.aliases or ca["alias_name"] in fake.collections:
                            return self.reply(409, {"status": {"error": "alias exists"}})
                        fake.aliases[ca["alias_name"]] = ca["collection_name"]
                return self.reply(200, {"result": True})
            self.reply(404, {})

        def do_GET(self):
            path = urlparse(self.path).path
            if path == "/collections":
                return self.reply(200, {"result": {"collections": [{"name": n} for n in fake.collections]}})
            if path == "/aliases":
                aliases = [{"alias_name": a, "collection_name": c} for a, c in fake.aliases.items()]
                return self.reply(200, {"result": {"aliases": aliases}})
            if path.startswith("/collections/"):
                name = path.split("/")[2]
                return self.reply(404, {"status": {"error": f"Not found: Collection `{name}` doesn't exist!"}})
            self.reply(404, {})

        def do_PUT(self):
            parts = urlparse(self.path).path.strip("/").split("/")
            b = self.body()
            if self.headers.get("api-key") not in (None, "store-key"):
                return self.reply(403, {"status": {"error": "bad api key"}})
            if len(parts) == 2 and parts[0] == "collections":
                if parts[1] in fake.collections:
                    return self.reply(409, {"status": {"error": "exists"}})
                fake.collections[parts[1]] = {"dim": b["vectors"]["size"], "points": {}}
                return self.reply(200, {"result": True})
            if len(parts) == 3 and parts[2] == "points":
                col = fake.collections.get(parts[1])
                if col is None:
                    return self.reply(404, {"status": {"error": "no collection"}})
                for p in b["points"]:
                    if len(p["vector"]) != col["dim"]:
                        return self.reply(400, {"status": {"error": "wrong dimension"}})
                    col["points"][p["id"]] = p
                return self.reply(200, {"result": {"status": "completed"}})
            self.reply(404, {})

        def do_DELETE(self):
            parts = urlparse(self.path).path.strip("/").split("/")
            if len(parts) == 2 and parts[1] in fake.collections:
                del fake.collections[parts[1]]
                for a, c in list(fake.aliases.items()):
                    if c == parts[1]:
                        del fake.aliases[a]
                return self.reply(200, {"result": True})
            self.reply(404, {"status": {"error": "not found"}})

    return H


class IngestTest(unittest.TestCase):
    def setUp(self):
        self.fake = Fake()
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler(self.fake))
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.dir = tempfile.TemporaryDirectory()
        self.data = self.dir.name
        self.write("guide.md", "# Guide\n\n" + "Gryvia schedules GPU jobs across clusters. " * 40)
        self.write("faq/answers.txt", "Quotas cap GPU hours per tenant.")
        self.write("faq/page.html", "<html><style>p{}</style><body><p>Budgets &amp; chargeback</p></body></html>")
        self.write("records.jsonl", '{"text": "first record"}\n\nnot json\n{"content": "second record"}\n{"x": 1}\n')
        self.write("logo.png", "\x89PNG")
        with open(os.path.join(self.data, "blob.txt"), "wb") as f:
            f.write(b"abc\x00def")
        self.logs = []
        self._sleep = ingest.time.sleep
        ingest.time.sleep = lambda s: None

    def tearDown(self):
        ingest.time.sleep = self._sleep
        self.server.shutdown()
        self.server.server_close()
        self.dir.cleanup()

    def write(self, rel, text):
        path = os.path.join(self.data, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            f.write(text)

    def cfg(self, **kw):
        c = {"collection": "kb", "run_id": "kb-ingest-1", "data_dir": self.data, "dataset_version": "v1",
             "chunk_size": 300, "chunk_overlap": 50, "batch": 4}
        c.update(kw)
        return c

    def run_ingest(self, store_key=None, **kw):
        emb = ingest.Embedder(self.url, "gk-test", "embed")
        store = ingest.Qdrant(self.url, store_key)
        return ingest.ingest(self.cfg(**kw), emb, store, log=lambda *a, **k: self.logs.append(a)), emb

    def test_ingest_creates_collection_and_alias(self):
        out, emb = self.run_ingest()
        self.assertEqual(out["documents"], 5)  # guide, answers, page, two jsonl records
        self.assertEqual(out["dimensions"], DIM)
        self.assertEqual(out["collection"], "kb")
        target = self.fake.aliases["kb"]
        self.assertTrue(target.startswith("kb__"))
        points = self.fake.collections[target]["points"]
        self.assertEqual(len(points), out["chunks"])
        sources = {p["payload"]["source"] for p in points.values()}
        self.assertEqual(
            sources, {"guide.md", "faq/answers.txt", "faq/page.html", "records.jsonl#1", "records.jsonl#4"}
        )
        page = next(p for p in points.values() if p["payload"]["source"] == "faq/page.html")
        self.assertEqual(page["payload"]["text"], "Budgets & chargeback")
        for p in points.values():
            self.assertEqual(p["vector"], fake_vector(p["payload"]["text"]))
            self.assertEqual(p["payload"]["datasetVersion"], "v1")
        self.assertTrue(all(len(c["input"]) <= 4 for c in self.fake.embed_calls))
        self.assertEqual(emb.tokens, out["chunks"] * 3)

    def test_reingest_switches_alias_and_drops_old(self):
        self.run_ingest()
        first = self.fake.aliases["kb"]
        self.write("new.txt", "A new document.")
        out, _ = self.run_ingest(run_id="kb-ingest-2", dataset_version="v2")
        second = self.fake.aliases["kb"]
        self.assertNotEqual(first, second)
        self.assertNotIn(first, self.fake.collections)
        self.assertEqual(out["documents"], 6)
        self.assertEqual(self.fake.alias_actions[-1][0], {"delete_alias": {"alias_name": "kb"}})

    def test_rerun_of_same_run_replaces_partial_collection(self):
        self.fake.collections["kb__" + ingest.run_suffix("kb-ingest-1")] = {"dim": 3, "points": {"x": {}}}
        self.fake.collections["kb__stale"] = {"dim": DIM, "points": {}}
        out, _ = self.run_ingest()
        self.assertEqual(len(self.fake.collections[self.fake.aliases["kb"]]["points"]), out["chunks"])
        self.assertNotIn("kb__stale", self.fake.collections)

    def test_plain_collection_with_alias_name_is_replaced(self):
        self.fake.collections["kb"] = {"dim": DIM, "points": {}}
        self.run_ingest()
        self.assertIn("kb", self.fake.aliases)
        self.assertNotIn("kb", self.fake.collections)

    def test_retries_transient_errors(self):
        self.fake.fail_once = 2
        out, _ = self.run_ingest()
        self.assertGreater(out["chunks"], 0)

    def test_embedding_error_is_reported(self):
        self.fake.fail_embed = "model embed is not served"
        with self.assertRaisesRegex(ingest.IngestError, "not served"):
            self.run_ingest()
        self.assertNotIn("kb", self.fake.aliases)

    def test_store_api_key(self):
        out, _ = self.run_ingest(store_key="store-key")
        self.assertGreater(out["chunks"], 0)
        with self.assertRaisesRegex(ingest.IngestError, "bad api key"):
            self.run_ingest(store_key="wrong", run_id="kb-ingest-3")

    def test_empty_dataset_fails(self):
        empty = tempfile.TemporaryDirectory()
        try:
            with self.assertRaisesRegex(ingest.IngestError, "no text documents"):
                self.run_ingest(data_dir=empty.name)
        finally:
            empty.cleanup()

    def test_embeds_while_reading(self):
        read = {"n": 0}

        def documents(root):
            for i in range(50):
                read["n"] += 1
                yield f"doc{i}.txt", f"document number {i}"

        seen = []

        class Recording(ingest.Embedder):
            def embed(self, texts):
                seen.append(read["n"])
                return super().embed(texts)

        original, ingest.read_documents = ingest.read_documents, documents
        try:
            out = ingest.ingest(self.cfg(batch=4), Recording(self.url, "gk-test", "embed"),
                                ingest.Qdrant(self.url), log=lambda *a, **k: None)
        finally:
            ingest.read_documents = original
        self.assertEqual((out["documents"], out["chunks"]), (50, 50))
        self.assertEqual(seen[0], 4)  # the first batch is embedded after four documents, not after all fifty
        self.assertEqual(len(self.fake.collections[self.fake.aliases["kb"]]["points"]), 50)

    def test_jsonl_has_no_file_cap_and_long_lines_are_skipped(self):
        old = ingest.MAX_FILE_BYTES, ingest.MAX_RECORD_BYTES
        ingest.MAX_FILE_BYTES, ingest.MAX_RECORD_BYTES = 100, 200
        try:
            records = [json.dumps({"text": f"record {i} " + "x" * 20}) for i in range(10)]
            records.insert(3, json.dumps({"text": "y" * 300}))
            self.write("big/corpus.jsonl", "\n".join(records) + "\n")
            self.write("big/tail.jsonl", json.dumps({"text": "z" * 300}))
            self.write("big/large.txt", "w " * 100)
            sources = [s for s, _ in ingest.read_documents(os.path.join(self.data, "big"))]
        finally:
            ingest.MAX_FILE_BYTES, ingest.MAX_RECORD_BYTES = old
        self.assertEqual(sources, [f"corpus.jsonl#{n}" for n in (1, 2, 3, 5, 6, 7, 8, 9, 10, 11)])

    def test_main_writes_termination_message(self):
        term = os.path.join(self.data, "..", "term-" + os.path.basename(self.data))
        env = {"GATEWAY_URL": self.url, "GRYVIA_LLM_KEY": "gk-test", "EMBED_MODEL": "embed", "STORE_URL": self.url,
               "COLLECTION": "kb", "RUN_ID": "r1", "DATA_DIR": self.data, "CHUNK_SIZE": "500", "CHUNK_OVERLAP": "0"}
        old_env, old_log = dict(os.environ), ingest.TERMINATION_LOG
        os.environ.update(env)
        ingest.TERMINATION_LOG = term
        try:
            self.assertEqual(ingest.main(), 0)
            with open(term) as f:
                out = json.load(f)
            self.assertEqual(out["dimensions"], DIM)
            self.assertEqual(out["documents"], 5)
            os.environ["GRYVIA_LLM_KEY"] = "wrong"
            self.assertEqual(ingest.main(), 1)
            with open(term) as f:
                self.assertIn("invalid API key", f.read())
        finally:
            os.environ.clear()
            os.environ.update(old_env)
            ingest.TERMINATION_LOG = old_log
            if os.path.exists(term):
                os.remove(term)


class ChunkTest(unittest.TestCase):
    def test_short_text_is_one_chunk(self):
        self.assertEqual(ingest.chunk("  hello   world ", 100, 10), ["hello world"])
        self.assertEqual(ingest.chunk("   ", 100, 10), [])

    def test_chunks_respect_size_and_overlap(self):
        words = " ".join(f"w{i}" for i in range(500))
        chunks = ingest.chunk(words, 100, 20)
        self.assertTrue(all(len(c) <= 100 for c in chunks))
        self.assertTrue(all(not c.startswith(" ") for c in chunks))
        for a, b in zip(chunks, chunks[1:]):
            self.assertTrue(set(a.split()[-2:]) & set(b.split()[:4]), (a, b))
        self.assertIn("w499", chunks[-1])

    def test_prefers_paragraph_breaks(self):
        paras = ["alpha " * 20, "beta " * 20, "gamma " * 20]
        chunks = ingest.chunk("\n\n".join(p.strip() for p in paras), 150, 0)
        self.assertEqual([c.split()[0] for c in chunks], ["alpha", "beta", "gamma"])
        self.assertTrue(all(len(set(c.split())) == 1 for c in chunks), chunks)

    def test_no_whitespace_still_terminates(self):
        chunks = ingest.chunk("x" * 1000, 100, 99)
        self.assertTrue(all(len(c) <= 100 for c in chunks))
        self.assertEqual("".join(c[0] for c in chunks[:1]), "x")


if __name__ == "__main__":
    unittest.main(verbosity=1)
