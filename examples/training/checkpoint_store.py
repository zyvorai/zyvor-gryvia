# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Crash-consistent, checksum-verified checkpoints for one writer per rank.

Immutable data is published before the atomic latest pointer. An interrupted save
leaves the previously committed checkpoint usable. Use durable shared storage.
"""
import hashlib
import json
import os
from pathlib import Path
import uuid


def _sync_dir(root):
    fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def save(root, payload, step, rank=0, world_size=1):
    root = Path(root) / str(rank)
    root.mkdir(parents=True, exist_ok=True)
    name = uuid.uuid4().hex + '.bin'
    data = root / name
    with data.open('xb') as stream:
        stream.write(payload)
        stream.flush()
        os.fsync(stream.fileno())
    _sync_dir(root)
    manifest = {'version': 1, 'blob': name, 'sha256': hashlib.sha256(payload).hexdigest(),
                'bytes': len(payload), 'step': step, 'rank': rank, 'world_size': world_size}
    temp = root / (uuid.uuid4().hex + '.tmp')
    try:
        with temp.open('x') as stream:
            json.dump(manifest, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, root / 'latest.json')
        _sync_dir(root)
    finally:
        temp.unlink(missing_ok=True)
    return manifest


def load(root, rank=0, world_size=1, max_bytes=1024 * 1024 * 1024):
    root = Path(root) / str(rank)
    pointer = root / 'latest.json'
    if not pointer.exists():
        return None
    if pointer.stat().st_size > 16384:
        raise ValueError('oversized checkpoint manifest')
    manifest = json.loads(pointer.read_text())
    name = manifest.get('blob', '')
    if (manifest.get('version') != 1 or Path(name).name != name or not name.endswith('.bin')
            or manifest.get('rank') != rank or manifest.get('world_size') != world_size
            or not isinstance(manifest.get('step'), int) or manifest['step'] < 0):
        raise ValueError('invalid checkpoint identity, format or world size')
    blob = root / name
    if blob.is_symlink() or not 0 <= blob.stat().st_size <= max_bytes:
        raise ValueError('invalid checkpoint blob')
    payload = blob.read_bytes()
    if len(payload) != manifest.get('bytes') or hashlib.sha256(payload).hexdigest() != manifest.get('sha256'):
        raise ValueError('checkpoint checksum mismatch')
    return payload, manifest
