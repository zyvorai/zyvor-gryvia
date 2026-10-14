#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Bounded native MLX generation with allocator hints and measured token throughput."""
import argparse,json,os,time

def parser():
    p=argparse.ArgumentParser();p.add_argument('--model',required=True);p.add_argument('--prompt',required=True);p.add_argument('--max-tokens',type=int,default=256);return p

def main():
    p=parser();a=p.parse_args()
    if not 1<=a.max_tokens<=8192:p.error('max-tokens must be 1..8192')
    import mlx.core as mx
    from mlx_lm import load,stream_generate
    budget=int(float(os.environ['GRYVIA_MEMORY_RESERVATION_GIB'])*1024**3)
    mx.set_memory_limit(budget);mx.set_cache_limit(min(budget//8,256*1024**2))
    started=time.monotonic();model,tokenizer=load(a.model);tokens=0
    for response in stream_generate(model,tokenizer,a.prompt,max_tokens=a.max_tokens):
        tokens+=1;print(json.dumps({'kind':'token','text':response.text},ensure_ascii=False),flush=True)
        if mx.get_active_memory()>budget:raise MemoryError('Managed MLX memory reservation exceeded')
    elapsed=time.monotonic()-started
    print(json.dumps({'kind':'metrics','tokens':tokens,'durationSeconds':elapsed,'tokensPerSecond':tokens/elapsed,'peakMetalBytes':mx.get_peak_memory()}),flush=True)
if __name__=='__main__':main()
