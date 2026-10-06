import asyncio
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace

import httpx
import pytest

spec = importlib.util.spec_from_file_location('benchmark_model', Path(__file__).parents[1] / 'benchmark-model.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def args():
    return SimpleNamespace(url='https://server.test/v1/chat/completions', model='model', name='test', gpu_type='A100',
                           engine='vllm', precision='fp16', concurrency=2, requests=4, max_tokens=128,
                           hourly_cost=2, currency='USD')


def test_measured_and_no_chunk_token_guessing():
    seen = []

    def response(request):
        seen.append(json.loads(request.content))
        content = 'data: ' + json.dumps({'choices': [{'delta': {'content': 'answer'}}]}) + '\n\n'
        content += 'data: ' + json.dumps({'usage': {'completion_tokens': 12}}) + '\n\ndata: [DONE]\n\n'
        return httpx.Response(200, text=content)

    result = asyncio.run(module.benchmark(args(), [{'messages': [{'role': 'user', 'content': 'q'}], 'expected': 'answer'}],
                                         httpx.MockTransport(response)))
    assert result['qualification'] == 'Measured'
    assert result['benchmark']['qualityScore'] == 1
    assert result['benchmark']['tokensPerSecond'] * result['windowSeconds'] == pytest.approx(48)
    assert all(s['stream_options']['include_usage'] for s in seen)


def test_missing_usage_blocks_qualification():
    transport = httpx.MockTransport(lambda request: httpx.Response(200, text=
        'data: {"choices":[{"delta":{"content":"words are not tokens"}}]}\n\ndata: [DONE]\n\n'))
    result = asyncio.run(module.benchmark(args(), [{'messages': [{'role': 'user', 'content': 'q'}], 'expected': 'x'}], transport))
    assert result['qualification'] == 'Blocked'
    assert 'benchmark' not in result


def test_http_errors_are_not_success():
    result = asyncio.run(module.benchmark(args(), [{'messages': [{'role': 'user', 'content': 'q'}], 'expected': 'x'}],
                                         httpx.MockTransport(lambda r: httpx.Response(503))))
    assert result['failedRequests'] == 4
    assert result['qualification'] == 'Blocked'
