# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Recoverable single-process PyTorch trainer with the Gryvia preStop protocol."""
import argparse
import json
import os
from pathlib import Path
import signal
import time
import torch
from checkpoint_worker import atomic_json, request_save
from pytorch_recovery import checkpoint, resume


def train(root, rank, steps, checkpoint_every):
    if int(os.getenv('WORLD_SIZE', '1')) != 1:
        raise ValueError('this example is single-process; distributed checkpoints need coordinated saves')
    device = 'cuda' if torch.cuda.is_available() else 'cpu'
    model = torch.nn.Linear(4, 1).to(device)
    optimizer = torch.optim.SGD(model.parameters(), lr=.01, momentum=.9)
    step = resume(root, model, optimizer, rank)
    print(f'resumed step={step}', flush=True)
    stopping = False
    def stop(*_):
        nonlocal stopping
        stopping = True
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    handled = None
    last = time.monotonic()
    while step < steps and not stopping:
        x = torch.rand(8, 4, device=device)
        optimizer.zero_grad()
        model(x).square().mean().backward()
        optimizer.step()
        step += 1
        request = root / f'request-{rank}.json'
        token = json.loads(request.read_text()).get('token') if request.exists() else None
        if (token and token != handled) or time.monotonic() - last >= checkpoint_every:
            checkpoint(root, model, optimizer, step, rank)
            if token and token != handled:
                atomic_json(root / f'ack-{rank}.json', {'token': token, 'step': step})
                handled = token
            last = time.monotonic()
        time.sleep(.01)
    checkpoint(root, model, optimizer, step, rank)

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--request', action='store_true')
    parser.add_argument('--timeout', type=float, default=90)
    parser.add_argument('--steps', type=int, default=1000000)
    parser.add_argument('--checkpoint-every', type=float, default=5)
    args = parser.parse_args()
    root = Path(os.environ['GRYVIA_CHECKPOINT_DIR']); root.mkdir(parents=True, exist_ok=True)
    rank = int(os.getenv('RANK', '0'))
    if args.request:
        request_save(root, str(rank), args.timeout)
    else:
        train(root, rank, args.steps, args.checkpoint_every)
