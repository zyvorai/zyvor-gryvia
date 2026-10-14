#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""MPS smoke/benchmark adapter; synchronizes GPU work before reporting duration."""
import argparse,os,time,json

def main():
    p=argparse.ArgumentParser();p.add_argument('--size',type=int,default=2048);p.add_argument('--iterations',type=int,default=10);a=p.parse_args()
    if not 1<=a.size<=8192 or not 1<=a.iterations<=1000:p.error('invalid benchmark dimensions')
    import torch
    if not torch.backends.mps.is_available():raise RuntimeError('MPS unavailable; no CPU fallback')
    # torch's denominator is the recommended Metal working set, not installed RAM.
    # Exact byte budgets require the local recommended working-set value.
    recommended=os.getenv('GRYVIA_METAL_WORKING_SET_BYTES')
    if recommended:
        budget=float(os.environ['GRYVIA_MEMORY_RESERVATION_GIB'])*1024**3
        torch.mps.set_per_process_memory_fraction(min(1,budget/float(recommended)))
    x=torch.randn(a.size,a.size,device='mps');y=torch.randn(a.size,a.size,device='mps')
    for _ in range(2):z=x@y
    torch.mps.synchronize();start=time.monotonic()
    for _ in range(a.iterations):z=x@y
    torch.mps.synchronize();duration=time.monotonic()-start
    print(json.dumps({'kind':'metrics','backend':'mps','iterations':a.iterations,'durationSeconds':duration,'allocatedBytes':torch.mps.current_allocated_memory()}),flush=True)
if __name__=='__main__':main()
