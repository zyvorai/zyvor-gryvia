# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Globally consistent checkpoints across ranks, on shared storage (ReadWriteMany).

checkpoint_store.py gives each rank its own atomic latest pointer, so after a failure rank 0 can resume at
step 20 while rank 1 only has step 10. This module adds the missing commit: a step is usable only after EVERY
rank has published its blob and rank 0 has written one COMMIT record that lists them all.

Protocol for step S (all ranks call save_global with the same step):
  1. rank r writes steps/S/rank-r.bin, then (atomically) steps/S/rank-r.json with its sha256, size, rank, world size
  2. rank 0 waits until all world_size rank manifests exist, then atomically writes steps/S/COMMIT (the list of
     every rank's sha256) and moves COMMITTED to S (never backwards)
  3. other ranks wait for COMMIT; they return only once the step is committed
A rank that dies, or a timeout, leaves S without COMMIT. Nobody ever resumes from it: load_global reads only the
step named by COMMITTED and checks each blob against the sha256 recorded in COMMIT.

What this does not do: elect a new rank 0, tolerate a changed world size (resharding), verify the contents
(only integrity), garbage-collect (see prune), or help on storage without atomic rename and fsync semantics.
A trainer still has to call save_global at a point where all ranks hold the same step (after an all-reduce
barrier). elastic_train.py uses it under a real torchrun (CPU, gloo) in the kind e2e; never run on GPUs.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import time

STEPS = 'steps'
COMMITTED = 'COMMITTED'


def _sync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _atomic_write(path, data):
    path = Path(path)
    tmp = path.with_name(path.name + '.' + str(os.getpid()) + '.tmp')
    try:
        with tmp.open('wb') as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
        _sync_dir(path.parent)
    finally:
        tmp.unlink(missing_ok=True)


def _step_dir(root, step):
    if not isinstance(step, int) or isinstance(step, bool) or step < 0:
        raise ValueError('step must be a non-negative integer')
    return Path(root) / STEPS / str(step)


def _wait(predicate, timeout, poll):
    deadline = time.monotonic() + timeout
    while True:
        value = predicate()
        if value:
            return value
        if time.monotonic() >= deadline:
            return None
        time.sleep(poll)


def _rank_manifest(sdir, rank):
    path = sdir / ('rank-%d.json' % rank)
    try:
        return json.loads(path.read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        return None  # missing, or not fully visible yet


def save_global(root, payload, step, rank, world_size, timeout=60.0, poll=0.05):
    """Publish this rank's payload for `step` and return only when the step is globally committed.

    Raises TimeoutError when the step could not be committed in time; the step is then ignored on resume.
    """
    if not 0 <= rank < world_size:
        raise ValueError('rank outside world size')
    sdir = _step_dir(root, step)
    sdir.mkdir(parents=True, exist_ok=True)
    blob = sdir / ('rank-%d.bin' % rank)
    _atomic_write(blob, payload)
    manifest = {'version': 1, 'step': step, 'rank': rank, 'world_size': world_size,
                'bytes': len(payload), 'sha256': hashlib.sha256(payload).hexdigest()}
    _atomic_write(sdir / ('rank-%d.json' % rank), json.dumps(manifest).encode())

    commit = sdir / 'COMMIT'
    if rank == 0:
        def all_in():
            found = [_rank_manifest(sdir, r) for r in range(world_size)]
            return found if all(m is not None for m in found) else None
        found = _wait(all_in, timeout, poll)
        if found is None:
            raise TimeoutError('step %d: not every rank published within %ss; not committed' % (step, timeout))
        if any(m.get('world_size') != world_size or m.get('step') != step for m in found):
            raise ValueError('ranks disagree about the world size or the step')
        record = {'version': 1, 'step': step, 'world_size': world_size,
                  'ranks': [{'rank': m['rank'], 'bytes': m['bytes'], 'sha256': m['sha256']} for m in found]}
        _atomic_write(commit, json.dumps(record).encode())
        current = latest_committed_step(root)
        if current is None or step > current:
            _atomic_write(Path(root) / COMMITTED, json.dumps({'step': step}).encode())
        return step
    if _wait(commit.exists, timeout, poll) is None:
        raise TimeoutError('step %d: rank 0 did not commit within %ss' % (step, timeout))
    return step


def latest_committed_step(root):
    try:
        data = json.loads((Path(root) / COMMITTED).read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        return None
    step = data.get('step')
    return step if isinstance(step, int) and not isinstance(step, bool) and step >= 0 else None


def load_global(root, rank, world_size, max_bytes=1024 * 1024 * 1024):
    """Return (payload, step) of the latest committed step for this rank, or None when nothing is committed.

    Raises ValueError for a world-size mismatch, a missing or corrupted blob, or a record that does not match.
    """
    step = latest_committed_step(root)
    if step is None:
        return None
    sdir = _step_dir(root, step)
    try:
        record = json.loads((sdir / 'COMMIT').read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        raise ValueError('COMMITTED names step %d but its COMMIT record is missing or unreadable' % step)
    if record.get('world_size') != world_size or record.get('step') != step:
        raise ValueError('checkpoint was committed for world size %s, resuming with %s' % (record.get('world_size'), world_size))
    mine = [r for r in record.get('ranks', []) if r.get('rank') == rank]
    if len(mine) != 1 or len(record.get('ranks', [])) != world_size:
        raise ValueError('commit record does not list this rank exactly once')
    blob = sdir / ('rank-%d.bin' % rank)
    if blob.is_symlink() or not blob.exists() or blob.stat().st_size > max_bytes:
        raise ValueError('invalid checkpoint blob for rank %d' % rank)
    payload = blob.read_bytes()
    if len(payload) != mine[0]['bytes'] or hashlib.sha256(payload).hexdigest() != mine[0]['sha256']:
        raise ValueError('checkpoint checksum mismatch for rank %d at step %d' % (rank, step))
    return payload, step


def load_replicated(root, max_bytes=1024 * 1024 * 1024):
    """Return (payload, step) of rank 0's blob at the latest committed step, or None when nothing is committed.

    For data-parallel training, where every rank holds the same state, so a run can resume with a different
    world size (an elastic job that lost or regained workers). The blob is checked against the COMMIT record.
    """
    step = latest_committed_step(root)
    if step is None:
        return None
    sdir = _step_dir(root, step)
    try:
        record = json.loads((sdir / 'COMMIT').read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        raise ValueError('COMMITTED names step %d but its COMMIT record is missing or unreadable' % step)
    first = [r for r in record.get('ranks', []) if r.get('rank') == 0]
    if record.get('step') != step or len(first) != 1:
        raise ValueError('commit record for step %d does not list rank 0 exactly once' % step)
    blob = sdir / 'rank-0.bin'
    if blob.is_symlink() or not blob.exists() or blob.stat().st_size > max_bytes:
        raise ValueError('invalid checkpoint blob for rank 0')
    payload = blob.read_bytes()
    if len(payload) != first[0]['bytes'] or hashlib.sha256(payload).hexdigest() != first[0]['sha256']:
        raise ValueError('checkpoint checksum mismatch for rank 0 at step %d' % step)
    return payload, step


def discard_uncommitted(root):
    """Delete every step directory above the committed step. Call it from rank 0 before the ranks resume (then
    barrier): a step that an earlier attempt left half-published would otherwise still hold that attempt's rank
    manifests, and rank 0 could commit them when the step is saved again. Returns the removed step numbers."""
    latest = latest_committed_step(root)
    steps_dir = Path(root) / STEPS
    if not steps_dir.is_dir():
        return []
    removed = sorted(int(d.name) for d in steps_dir.iterdir()
                     if d.name.isdigit() and (latest is None or int(d.name) > latest))
    for n in removed:
        shutil.rmtree(steps_dir / str(n), ignore_errors=True)
    return removed


def prune(root, keep=2):
    """Delete all but the newest `keep` committed steps and every uncommitted step older than the newest
    committed one. Run it from one place (for example rank 0 after a commit), never concurrently with itself.
    Returns the removed step numbers."""
    latest = latest_committed_step(root)
    steps_dir = Path(root) / STEPS
    if latest is None or not steps_dir.is_dir():
        return []
    committed, stale = [], []
    for d in steps_dir.iterdir():
        if not d.name.isdigit():
            continue
        n = int(d.name)
        (committed if (d / 'COMMIT').exists() else stale).append(n)
    keep_set = set(sorted(committed)[-max(keep, 1):]) | {latest}
    removed = [n for n in committed if n not in keep_set] + [n for n in stale if n < latest]
    for n in removed:
        shutil.rmtree(steps_dir / str(n), ignore_errors=True)
    return sorted(removed)
