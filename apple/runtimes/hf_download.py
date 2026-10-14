#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Bounded model download into the managed models directory. Streams progress, verifies size cap, records sha256.

Only https on huggingface.co / hf.co is accepted as the origin. The Authorization header (HF_TOKEN from the
environment) is never forwarded to a different host after a redirect."""
import argparse
import hashlib
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _common import HOSTS, confine, emit, models_root  # noqa: E402


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urllib.parse.urlsplit(newurl)
        if target.scheme != "https":
            raise urllib.error.URLError("redirect to non-https refused")
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None and target.hostname != urllib.parse.urlsplit(req.full_url).hostname:
            new.remove_header("Authorization")
        return new


def build_url(a) -> str:
    if a.url:
        return a.url
    prefix = "datasets/" if getattr(a, "dataset", False) else ""
    return f"https://huggingface.co/{prefix}{a.repo}/resolve/{urllib.parse.quote(a.revision)}/{urllib.parse.quote(a.file)}"


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--repo"); p.add_argument("--file"); p.add_argument("--revision", default="main")
    p.add_argument("--url"); p.add_argument("--max-gib", type=float, default=20); p.add_argument("--dataset", action="store_true", help="repo is a Hugging Face dataset")
    a = p.parse_args(argv)
    if not a.url and not (a.repo and a.file):
        p.error("--url or --repo and --file required")
    url = build_url(a)
    u = urllib.parse.urlsplit(url)
    if u.scheme != "https" or u.hostname not in HOSTS:
        p.error("origin must be https on " + ", ".join(HOSTS))
    if a.file and (".." in a.file or a.file.startswith("/")):
        p.error("invalid file")
    name = Path(urllib.parse.unquote(u.path)).name
    dest_dir = models_root() / ("_datasets" if a.dataset else "") / (a.repo.replace("/", "__") if a.repo else "url")
    dest = confine(str(dest_dir / name))
    dest.parent.mkdir(parents=True, exist_ok=True)
    cap = int(a.max_gib * 1024**3)
    headers = {"User-Agent": "gryvia-apple/0.2"}
    token = os.getenv("HF_TOKEN")
    if token:
        headers["Authorization"] = "Bearer " + token
    opener = urllib.request.build_opener(SafeRedirect)
    emit(kind="progress", stage="connect", url=url)
    try:
        resp = opener.open(urllib.request.Request(url, headers=headers), timeout=60)
    except urllib.error.HTTPError as e:
        emit(kind="error", message=f"HTTP {e.code} fetching {u.path}" + (" (gated model: set HF_TOKEN)" if e.code in (401, 403) else ""))
        return 1
    except (urllib.error.URLError, TimeoutError) as e:
        emit(kind="error", message=f"cannot reach host: {getattr(e, 'reason', e)}")
        return 1
    total = int(resp.headers.get("Content-Length") or 0)
    if total and total > cap:
        emit(kind="error", message=f"file is {total / 1024**3:.1f} GiB, above the {a.max_gib} GiB cap")
        return 1
    part = dest.with_suffix(dest.suffix + ".part")
    digest, done, last = hashlib.sha256(), 0, 0.0
    with open(part, "wb") as f:
        while True:
            chunk = resp.read(1024 * 1024)
            if not chunk:
                break
            done += len(chunk)
            if done > cap:
                f.close(); part.unlink(missing_ok=True)
                emit(kind="error", message=f"download exceeded the {a.max_gib} GiB cap")
                return 1
            digest.update(chunk); f.write(chunk)
            if time.monotonic() - last > 1:
                last = time.monotonic()
                emit(kind="progress", stage="download", bytes=done, total=total, pct=round(done / total * 100, 1) if total else None)
    os.replace(part, dest)
    extra = {}
    if a.dataset and dest.suffix in (".jsonl", ".json", ".txt", ".csv"):
        with open(dest, "rb") as f:
            extra["rows"] = sum(1 for line in f if line.strip())
    emit(kind="result", path=str(dest), sha256=digest.hexdigest(), bytes=done, source=url, **extra)
    return 0


if __name__ == "__main__":
    sys.exit(main())
