# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""CPU/GPU PyTorch state recovery helpers; each rank owns its checkpoint.

Distributed workers must save a mutually agreed step; this helper does not
coordinate barriers or reshard optimizer state after world-size changes.
"""
import io
import random
import torch
from checkpoint_store import save, load


def checkpoint(root, model, optimizer, step, rank=0, world_size=1):
    stream = io.BytesIO()
    state = {'model': model.state_dict(), 'optimizer': optimizer.state_dict(), 'step': step,
             'python_rng': random.getstate(), 'torch_rng': torch.get_rng_state()}
    if torch.cuda.is_available():
        state['cuda_rng'] = torch.cuda.get_rng_state_all()
    torch.save(state, stream)
    return save(root, stream.getvalue(), step, rank, world_size)


def resume(root, model, optimizer, rank=0, world_size=1):
    restored = load(root, rank, world_size)
    if restored is None:
        return 0
    payload, manifest = restored
    state = torch.load(io.BytesIO(payload), map_location='cpu', weights_only=True)
    if state['step'] != manifest['step']:
        raise ValueError('checkpoint step mismatch')
    model.load_state_dict(state['model'])
    optimizer.load_state_dict(state['optimizer'])
    random.setstate(state['python_rng'])
    torch.set_rng_state(state['torch_rng'])
    if 'cuda_rng' in state and torch.cuda.is_available():
        torch.cuda.set_rng_state_all(state['cuda_rng'])
    return state['step']
