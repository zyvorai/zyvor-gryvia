"""The gateway preflight and the ai-operator's Go port (pkg/preflight) must agree on the shared golden cases."""

import json
import pathlib

import pytest

from intelligence.engine import preflight
from intelligence.models import Preflight

GOLDEN = pathlib.Path(__file__).resolve().parents[3] / "operators/ai-operator/pkg/preflight/testdata/golden.json"


@pytest.mark.parametrize("case", json.loads(GOLDEN.read_text()), ids=lambda c: c["name"])
def test_golden_case(case):
    pools = [{"hourlyRate": 1, "interconnect": "none", **p} for p in case["pools"]]
    # Quota, storage and budget are the admission gate's job in Go; supply them so only the shared checks decide.
    body = Preflight(**case["input"], durationHours=1, quotaGPUs=1048576, storageReady=True, pools=pools)
    result = preflight(body)
    assert result["state"] == case["expect"]["state"]
    assert result["estimatedMemoryGiBPerGPU"] == pytest.approx(case["expect"]["perGPU"], abs=0.001)
    assert [c["pool"] for c in result["candidates"] if c["eligible"]] == case["expect"]["eligible"]
