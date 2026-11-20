#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Read-only hardware qualification; reports blocked checks without inventing results."""
import argparse
import json
from pathlib import Path
import shutil
import subprocess
import sys
import time


def run(name, command, timeout=120):
    if not shutil.which(command[0]):
        return {'check': name, 'status': 'blocked', 'reason': command[0] + ' unavailable'}
    started = time.monotonic()
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
        return {'check': name, 'status': 'passed' if result.returncode == 0 else 'failed',
                'seconds': time.monotonic() - started,
                'output': (result.stdout + result.stderr)[-8192:]}
    except subprocess.TimeoutExpired:
        return {'check': name, 'status': 'failed', 'reason': 'timeout'}


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--gpu', action='store_true')
    p.add_argument('--rdma', action='store_true')
    p.add_argument('--storage-file', type=Path, help='Existing file to read with fio; never writes it')
    p.add_argument('--nccl-command', nargs=argparse.REMAINDER, help='Last option: explicit nccl-tests argv, no shell')
    p.add_argument('--output', type=Path, required=True)
    a = p.parse_args(); results = []
    if a.gpu:
        results.append(run('GPU inventory', ['nvidia-smi', '--query-gpu=name,uuid,driver_version,memory.total,temperature.gpu', '--format=csv']))
    if a.rdma:
        results.append(run('RDMA inventory', ['ibv_devinfo']))
    if a.storage_file:
        if not a.storage_file.is_file():
            results.append({'check': 'storage', 'status': 'blocked', 'reason': 'existing regular file required'})
        else:
            results.append(run('storage read', ['fio', '--readonly', '--name=gryvia-read', '--rw=read', '--bs=1M', '--direct=1', '--time_based', '--runtime=10', '--output-format=json', '--filename=' + str(a.storage_file.resolve())]))
    if a.nccl_command:
        results.append(run('NCCL', a.nccl_command, timeout=300))
    if not results:
        results.append({'check': 'hardware', 'status': 'blocked', 'reason': 'select qualification checks'})
    a.output.write_text(json.dumps({'checks': results}, indent=2))
    return 0 if all(r['status'] == 'passed' for r in results) else 1

if __name__ == '__main__': sys.exit(main())
