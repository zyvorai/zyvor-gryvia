#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Download a Hugging Face model revision into a shared model cache (a PVC), once.

  python3 download.py --model-id Qwen/Qwen3-8B --revision <sha> --cache-dir /models/cache

A finished download leaves a marker with the revision; running again for the same revision does nothing. Gated
models need HF_TOKEN (the GryviaModelWatch tokenSecretRef can feed it through the job's env). Outputs: path (the
local directory) and subPath (relative to --pvc-root, for a GryviaModelRegistry entry).
"""
import argparse
import json
import os
import sys

from outputs import is_rank_zero, write_outputs

MARKER = ".gryvia-download.json"
DEFAULT_PATTERNS = ["*.json", "*.safetensors", "*.model", "*.txt", "*.tiktoken", "tokenizer*", "*.py"]


def target_dir(cache_dir, model_id, revision):
    org, name = model_id.split("/", 1)
    return os.path.join(cache_dir, org, name, revision or "main")


def already_downloaded(path, revision):
    try:
        with open(os.path.join(path, MARKER)) as f:
            return json.load(f).get("revision") == revision
    except (OSError, ValueError):
        return False


def download(model_id, revision, cache_dir, patterns=None, snapshot=None):
    """Download (or reuse) the revision; snapshot is huggingface_hub.snapshot_download, injectable for tests."""
    path = target_dir(cache_dir, model_id, revision)
    if already_downloaded(path, revision):
        print(f"{model_id}@{revision} already in {path}", flush=True)
        return path
    if snapshot is None:
        from huggingface_hub import snapshot_download as snapshot
    os.makedirs(path, exist_ok=True)
    snapshot(repo_id=model_id, revision=revision or None, local_dir=path, allow_patterns=patterns or DEFAULT_PATTERNS)
    with open(os.path.join(path, MARKER), "w") as f:
        json.dump({"model": model_id, "revision": revision}, f)
    return path


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--model-id", default=os.environ.get("MODEL_ID"), required=not os.environ.get("MODEL_ID"))
    p.add_argument("--revision", default=os.environ.get("MODEL_REVISION", ""))
    p.add_argument("--cache-dir", default="/models/cache")
    p.add_argument("--pvc-root", default="/models", help="mount path of the PVC, to report subPath")
    a = p.parse_args(argv)
    path = download(a.model_id, a.revision, a.cache_dir)
    if is_rank_zero():
        write_outputs({"path": path, "subPath": os.path.relpath(path, a.pvc_root), "revision": a.revision or "main"})
    return 0


if __name__ == "__main__":
    sys.exit(main())
