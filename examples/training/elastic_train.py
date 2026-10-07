"""A small elastic data-parallel trainer for `torchrun` on CPU (gloo), used by the kind e2e.

Launch it the way docs/elastic-training.md says, from every pod of an elastic GryviaAIJob:

  torchrun --nnodes=$NNODES --nproc_per_node=$NPROC_PER_NODE --rdzv_backend=c10d \
    --rdzv_endpoint=$MASTER_ADDR:$MASTER_PORT --rdzv_id=$JOB --max_restarts=3 elastic_train.py

It fits y = x @ W with DistributedDataParallel on a fixed batch of 8 samples per step, split evenly across the
ranks, so the averaged gradient is the same whatever the world size. Every CHECKPOINT_EVERY steps all ranks save
model and optimizer (Adam) state with torch.distributed.checkpoint and commit the step (dcp_checkpoint.save, on
the COMMIT/COMMITTED protocol of coordinated_checkpoint) under CHECKPOINT_DIR. When torchrun restarts the workers
after a member was lost, or the group re-forms with another size, each rank loads the last committed step,
resharded to the new world size (dcp_checkpoint.load); a directory holding an older opaque-blob checkpoint is
still resumed from rank 0's blob. Rank 0 writes
DONE (JSON: steps, final loss, world size, restarts, resumedFrom) next to the checkpoints when training finishes.
`restarts` is TORCHELASTIC_RESTART_COUNT: failure restarts only; a group re-formed because a node joined keeps it.

Environment: CHECKPOINT_DIR (required), TOTAL_STEPS (80), CHECKPOINT_EVERY (5), STEP_SECONDS (0.5),
COLLECTIVE_TIMEOUT (60 seconds).
"""
from datetime import timedelta
import io
import json
import os
from pathlib import Path
import time

import torch
import torch.distributed as dist
from torch.nn.parallel import DistributedDataParallel as DDP

import coordinated_checkpoint as cc
import dcp_checkpoint

BATCH = 8
TRUE_W = torch.tensor([[1.0, -2.0, 3.0, 0.5]])


def batch(step):
    g = torch.Generator().manual_seed(step)
    x = torch.randn(BATCH, 4, generator=g)
    return x, x @ TRUE_W.T


def resume(root, model, opt):
    """Return (step, world size it was saved with) after loading the last committed step, or (0, None)."""
    step = cc.latest_committed_step(root)
    if step is None:
        return 0, None
    try:
        dcp_checkpoint.read_commit(root, step)
    except ValueError:
        payload, step = cc.load_replicated(root)  # an opaque-blob checkpoint from before DCP
        state = torch.load(io.BytesIO(payload))
        model.module.load_state_dict(state['model'])
        return step, None
    return dcp_checkpoint.load(root, model, opt)


def main():
    root = os.environ['CHECKPOINT_DIR']
    total = int(os.environ.get('TOTAL_STEPS', '80'))
    every = int(os.environ.get('CHECKPOINT_EVERY', '5'))
    pause = float(os.environ.get('STEP_SECONDS', '0.5'))
    restarts = int(os.environ.get('TORCHELASTIC_RESTART_COUNT', '0'))
    # a peer that disappears mid all-reduce only surfaces as a collective timeout (30 min by default)
    collective_timeout = int(os.environ.get('COLLECTIVE_TIMEOUT', '60'))

    dist.init_process_group('gloo', timeout=timedelta(seconds=collective_timeout))
    rank, world = dist.get_rank(), dist.get_world_size()
    if BATCH % world:
        raise SystemExit('world size %d does not divide the batch of %d' % (world, BATCH))
    Path(root).mkdir(parents=True, exist_ok=True)
    if rank == 0:
        dropped = cc.discard_uncommitted(root)
        if dropped:
            print('discarded uncommitted steps %s' % dropped, flush=True)
    dist.barrier()

    torch.manual_seed(0)
    model = DDP(torch.nn.Linear(4, 1, bias=False))
    opt = torch.optim.Adam(model.parameters(), lr=0.05)
    start, saved_world = resume(root, model, opt)
    print('rank %d of %d: starting after step %d (saved by world %s, restart %d)'
          % (rank, world, start, saved_world, restarts), flush=True)

    loss = None
    for step in range(start + 1, total + 1):
        x, y = batch(step)
        xs, ys = x.chunk(world)[rank], y.chunk(world)[rank]
        opt.zero_grad()
        torch.nn.functional.mse_loss(model(xs), ys).backward()  # DDP averages the gradients over the ranks
        opt.step()
        with torch.no_grad():
            loss = torch.nn.functional.mse_loss(model.module(x), y).item()
        if step % every == 0:
            dcp_checkpoint.save(root, step, model, opt)
            if rank == 0:
                cc.prune(root, keep=2)
                print('committed step %d (world %d, loss %.6f)' % (step, world, loss), flush=True)
        time.sleep(pause)

    dist.barrier()
    if rank == 0:
        done = {'steps': total, 'loss': loss, 'world': world, 'restarts': restarts, 'resumedFrom': start,
                'resumedFromWorld': saved_world}
        Path(root, 'DONE').write_text(json.dumps(done))
        print('done: %s' % json.dumps(done), flush=True)
    dist.destroy_process_group()


if __name__ == '__main__':
    main()
