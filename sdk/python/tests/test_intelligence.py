import asyncio
import hashlib
from pathlib import Path

import httpx
import pytest

from gryvia import Gryvia
from gryvia.artifact_cache import materialize
from gryvia.training_telemetry import TrainingTelemetry


def test_sdk_intelligence_calls_and_area_guard():
    client = Gryvia(api_url='https://gateway.test', token='token')
    client._http = httpx.AsyncClient(base_url='https://gateway.test', transport=httpx.MockTransport(
        lambda request: httpx.Response(200, json={'path': request.url.path})))
    result = asyncio.run(client.intelligence.analyze('capacity', {'pools': [], 'demand': []}))
    assert result['path'] == '/api/intelligence/capacity'
    with pytest.raises(ValueError):
        asyncio.run(client.intelligence.analyze('arbitrary/path', {}))
    with pytest.raises(ValueError):
        asyncio.run(client.intelligence.transition('id', 'approve-and-run'))
    asyncio.run(client.close())


def test_training_timings_multiple_phases_and_missing_metrics():
    t = TrainingTelemetry(rank=2)
    t.record('step', 1)
    t.record('step', 1)
    t.record('data', .2)
    t.record('data', .3)
    output = t.snapshot()
    assert output['dataWaitSeconds'] == .25
    assert output['collectiveSeconds'] is None and output['gpuUtilization'] is None
    with pytest.raises(ValueError):
        t.record('step', float('nan'))
    t.reset()
    with pytest.raises(ValueError):
        t.snapshot()


def test_verified_cache_hit_mismatch_and_size(tmp_path):
    source = tmp_path / 'data'
    source.write_bytes(b'dataset')
    digest = hashlib.sha256(b'dataset').hexdigest()
    output = materialize(source, tmp_path / 'cache', digest)
    assert output['cacheHit'] is False
    assert materialize(source, tmp_path / 'cache', digest)['cacheHit'] is True
    # A corrupted hit is reverified and repaired from the source.
    artifact = Path(output['path'])
    artifact.chmod(0o600)
    artifact.write_bytes(b'corrupt')
    assert materialize(source, tmp_path / 'cache', digest)['cacheHit'] is False
    with pytest.raises(ValueError):
        materialize(source, tmp_path / 'cache', 'b' * 64)
    assert not (tmp_path / 'cache/sha256' / ('b' * 64)).exists()
    with pytest.raises(ValueError):
        materialize(source, tmp_path / 'small-cache', digest, max_bytes=2)
    assert not list((tmp_path / 'small-cache/sha256').glob('.artifact-*'))


def test_cache_rejects_symlink_source(tmp_path):
    data = tmp_path / 'data'
    data.write_bytes(b'x')
    symlink = tmp_path / 'link'
    symlink.symlink_to(data)
    with pytest.raises(OSError):
        materialize(symlink, tmp_path / 'cache', hashlib.sha256(b'x').hexdigest())
