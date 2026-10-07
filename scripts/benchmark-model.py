#!/usr/bin/env python3
"""Measure an OpenAI-compatible streaming server; emit a laboratory observation.

Uses server-reported completion_tokens, never text length or chunk count as tokens.
Quality is an explicit normalized exact-match suite, not a general model-quality score.
Authorization is read from GRYVIA_BENCHMARK_TOKEN and never saved in a report.
"""
import argparse
import asyncio
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import time
from urllib.parse import urlparse

import httpx


def percentile(values, fraction=.95):
    values = sorted(values)
    return values[max(0, math.ceil(len(values) * fraction) - 1)] if values else None


async def measure(client, url, model, prompt, token, max_tokens):
    start = time.perf_counter()
    first, text, tokens = None, '', None
    headers = {"Authorization": f"Bearer {token}"} if token else {}
    try:
        async with client.stream('POST', url, headers=headers, json={
                'model': model, 'messages': prompt['messages'], 'max_tokens': max_tokens,
                'temperature': 0, 'stream': True, 'stream_options': {'include_usage': True}}) as response:
            if response.status_code != 200:
                return {'ok': False, 'error': 'HTTP %d' % response.status_code}
            async for line in response.aiter_lines():
                if not line.startswith('data:'):
                    continue
                data = line[5:].strip()
                if data == '[DONE]':
                    break
                if len(data) > 1024 * 1024:
                    raise ValueError('oversized stream event')
                item = json.loads(data)
                if item.get('error'):
                    return {'ok': False, 'error': 'server stream error'}
                value = (item.get('usage') or {}).get('completion_tokens')
                if type(value) is int and 0 <= value <= 1000000000:
                    tokens = value
                for choice in item.get('choices') or []:
                    delta = (choice.get('delta') or {}).get('content')
                    if isinstance(delta, str) and delta:
                        if first is None:
                            first = time.perf_counter() - start
                        text += delta
                        if len(text) > 1024 * 1024:
                            raise ValueError('oversized completion')
        if first is None:
            return {'ok': False, 'error': 'no content delivered'}
        return {'ok': True, 'ttftMs': first * 1000, 'completionTokens': tokens,
                'qualityMatch': ' '.join(text.split()).casefold() == ' '.join(prompt['expected'].split()).casefold()}
    except (httpx.HTTPError, ValueError, TypeError, KeyError) as exc:
        return {'ok': False, 'error': type(exc).__name__}


async def benchmark(args, corpus, transport=None):
    if not corpus or len(corpus) > 10000 or any(not isinstance(p, dict) or not isinstance(p.get('messages'), list)
                                              or not p['messages'] or not isinstance(p.get('expected'), str) for p in corpus):
        raise ValueError('corpus needs 1-10000 entries with messages and expected text')
    if not 1 <= args.concurrency <= 32 or not 1 <= args.requests <= 10000 or not 1 <= args.max_tokens <= 100000:
        raise ValueError('concurrency/requests/max_tokens outside bounded ranges')
    if not math.isfinite(args.hourly_cost) or args.hourly_cost < 0:
        raise ValueError('invalid hourly cost')
    parsed = urlparse(args.url)
    if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username or parsed.password:
        raise ValueError('supply an HTTP(S) URL without embedded credentials')
    token = os.environ.get('GRYVIA_BENCHMARK_TOKEN', '')
    semaphore = asyncio.Semaphore(args.concurrency)
    timeout = httpx.Timeout(120, connect=10)
    async with httpx.AsyncClient(timeout=timeout, follow_redirects=False, trust_env=False, transport=transport) as client:
        async def one(index):
            async with semaphore:
                return await measure(client, args.url, args.model, corpus[index % len(corpus)], token, args.max_tokens)
        start = time.perf_counter()
        samples = await asyncio.gather(*(one(i) for i in range(args.requests)))
        elapsed = time.perf_counter() - start
    successes = [s for s in samples if s['ok']]
    missing = sum(s['completionTokens'] is None for s in successes)
    tokens = sum(s['completionTokens'] or 0 for s in successes)
    result = {'qualification': 'Blocked', 'requests': args.requests, 'successfulRequests': len(successes),
              'failedRequests': args.requests - len(successes), 'missingTokenUsage': missing,
              'errors': [s['error'] for s in samples if not s['ok']][:100],
              'windowSeconds': elapsed, 'observedAt': datetime.now(timezone.utc).isoformat(),
              'notes': ['TTFT measures first visible content, including client/network time.',
                        'No inter-token latency is inferred from SSE chunk spacing.',
                        'Quality is normalized exact-match on the supplied expected outputs.']}
    if successes and missing == 0 and tokens > 0:
        result['qualification'] = 'Measured'
        result['benchmark'] = {
            'name': args.name, 'model': args.model, 'gpuType': args.gpu_type, 'engine': args.engine,
            'precision': args.precision, 'concurrency': args.concurrency,
            'workloadDigest': hashlib.sha256(json.dumps(corpus, sort_keys=True, separators=(',', ':')).encode()).hexdigest(),
            'qualitySuite': 'normalized-exact-match-v1',
            'qualityScore': sum(s['qualityMatch'] for s in successes) / len(successes),
            'tokensPerSecond': tokens / elapsed, 'ttftP95Ms': percentile([s['ttftMs'] for s in successes]),
            'hourlyCost': args.hourly_cost, 'currency': args.currency, 'successfulRequests': len(successes),
            'failedRequests': args.requests - len(successes), 'source': 'openai-http-stream',
            'windowSeconds': elapsed, 'observedAt': result['observedAt']}
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ('url', 'corpus', 'model', 'name', 'gpu-type', 'precision', 'output'):
        parser.add_argument('--' + flag, required=True)
    parser.add_argument('--engine', choices=['vllm', 'triton', 'tensorrt-llm', 'llama.cpp'], required=True)
    parser.add_argument('--hourly-cost', type=float, required=True)
    parser.add_argument('--currency', default='USD')
    parser.add_argument('--concurrency', type=int, default=4)
    parser.add_argument('--requests', type=int, default=100)
    parser.add_argument('--max-tokens', type=int, default=128)
    args = parser.parse_args()
    path = Path(args.corpus)
    if path.stat().st_size > 4 * 1024 * 1024:
        parser.error('corpus exceeds 4 MiB')
    try:
        result = asyncio.run(benchmark(args, json.loads(path.read_text())))
    except (ValueError, OSError) as exc:
        parser.error(str(exc))
    Path(args.output).write_text(json.dumps(result, indent=2) + '\n')
    print(result['qualification'])
    return 0 if result['qualification'] == 'Measured' else 2


if __name__ == '__main__':
    raise SystemExit(main())
