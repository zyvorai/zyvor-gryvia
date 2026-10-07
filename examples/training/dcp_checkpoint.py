"""Sharded model + optimizer checkpoints with torch.distributed.checkpoint (DCP), resumable at another world size.

coordinated_checkpoint.py commits opaque per-rank blobs, so a run can only resume with the same world size (or,
for pure data parallelism, from rank 0's replica). This module stores the model and optimizer state through DCP
instead: every rank writes its shards of steps/S/dcp/ and DCP records, in its .metadata file, which global
tensor regions each file holds. Loading at a different world size (or with another FSDP sharding) reads exactly
the regions each rank now needs, so optimizer state such as Adam moments is resharded, not just copied.

It reuses the commit protocol of coordinated_checkpoint.py: a step counts only after rank 0 has written
steps/S/COMMIT, listing the sha256 of every DCP file, and moved COMMITTED to S. A rank that dies mid-save
leaves S without COMMIT and resume uses the previous committed step; discard_uncommitted and prune work as before.

    save(root, step, model, optimizer)          # collective: every rank calls it at the same step
    load(root, model, optimizer) -> (step, saved_world_size) or None

`model` may be a plain module, DDP, or FSDP/FSDP2 (fully_shard); state dicts are taken with
torch.distributed.checkpoint.state_dict.get_state_dict/set_state_dict, which use fully qualified parameter
names, so a DDP checkpoint loads into an unwrapped or differently wrapped model. Tested with multiple CPU
processes (gloo, DDP) saving at world size 4 and loading at 2 and 3 (test_dcp_checkpoint.py); never on GPUs.
"""
import json
from pathlib import Path

import torch.distributed as dist
import torch.distributed.checkpoint as dcp
from torch.distributed.checkpoint.state_dict import get_state_dict, set_state_dict

from checkpoint_contract import file_digest, verify_dcp_files
import coordinated_checkpoint as cc

FORMAT = 'dcp'
DATA_DIR = 'dcp'


def _rank_world():
    if dist.is_available() and dist.is_initialized():
        return dist.get_rank(), dist.get_world_size()
    return 0, 1


def _barrier():
    if dist.is_available() and dist.is_initialized():
        dist.barrier()


def _manifest(ddir, max_bytes):
    files = []
    for p in sorted(ddir.iterdir()):
        if p.is_symlink() or not p.is_file():
            raise ValueError('unexpected entry %s in the DCP directory' % p.name)
        size, sha = file_digest(p, max_bytes)
        files.append({'name': p.name, 'bytes': size, 'sha256': sha})
    if not any(f['name'] == '.metadata' for f in files):
        raise ValueError('DCP directory has no .metadata file')
    return files


def read_commit(root, step):
    """Return the COMMIT record of a committed DCP step. Raises ValueError when it is missing or not DCP."""
    try:
        record = json.loads((cc._step_dir(root, step) / 'COMMIT').read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        raise ValueError('COMMITTED names step %d but its COMMIT record is missing or unreadable' % step)
    if record.get('format') != FORMAT or record.get('step') != step:
        raise ValueError('step %d is not a DCP checkpoint (use coordinated_checkpoint.load_global)' % step)
    return record


def save(root, step, model, optimizer, max_bytes=1024 ** 4):
    """Collectively save model and optimizer state for `step`; return once the step is committed on every rank.

    Raises ValueError when the step was already committed. The step is never committed if any rank fails.
    """
    rank, world = _rank_world()
    sdir = cc._step_dir(root, step)
    ddir = sdir / DATA_DIR
    if (sdir / 'COMMIT').exists():
        raise ValueError('step %d is already committed' % step)
    ddir.mkdir(parents=True, exist_ok=True)
    model_sd, optim_sd = get_state_dict(model, optimizer)
    # dcp.save returns on every rank only after the coordinator (rank 0) has written .metadata
    dcp.save({'model': model_sd, 'optim': optim_sd}, checkpoint_id=str(ddir))
    if rank == 0:
        record = {'version': 1, 'format': FORMAT, 'step': step, 'world_size': world,
                  'files': _manifest(ddir, max_bytes)}
        cc._atomic_write(sdir / 'COMMIT', json.dumps(record).encode())
        current = cc.latest_committed_step(root)
        if current is None or step > current:
            cc._atomic_write(Path(root) / cc.COMMITTED, json.dumps({'step': step}).encode())
    _barrier()
    return step


def load(root, model, optimizer, verify=True, max_bytes=1024 ** 4):
    """Load the latest committed DCP step into model and optimizer, resharding to the current world size.

    Returns (step, world size the step was saved with), or None when nothing is committed. With verify, rank 0
    checks every file against the COMMIT record first and all ranks raise ValueError if it does not match.
    """
    step = cc.latest_committed_step(root)
    if step is None:
        return None
    record = read_commit(root, step)
    ddir = cc._step_dir(root, step) / DATA_DIR
    if verify:
        rank, _ = _rank_world()
        error = [None]
        if rank == 0:
            try:
                verify_dcp_files(ddir, record.get('files'), max_bytes)
            except ValueError as e:
                error[0] = str(e)
        if dist.is_available() and dist.is_initialized():
            dist.broadcast_object_list(error, src=0)
        if error[0]:
            raise ValueError(error[0])
    model_sd, optim_sd = get_state_dict(model, optimizer)
    state = {'model': model_sd, 'optim': optim_sd}
    dcp.load(state, checkpoint_id=str(ddir))
    set_state_dict(model, optimizer, model_state_dict=state['model'], optim_state_dict=state['optim'])
    return step, record.get('world_size')
