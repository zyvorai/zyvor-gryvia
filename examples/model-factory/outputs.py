# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Report step outputs to a GryviaWorkflow.

The workflow controller reads a JSON object of strings from the termination message of a step's rank-0 pod and
exposes it as {{steps.<step>.outputs.<key>}}. Kubernetes keeps at most 4096 bytes of that message, and the
controller drops keys outside [A-Za-z0-9_-] (at most 63 characters).
"""
import json
import os
import re

TERMINATION_LOG = os.environ.get("GRYVIA_TERMINATION_LOG", "/dev/termination-log")
MAX_BYTES = 4096
_KEY = re.compile(r"^[A-Za-z0-9_-]{1,63}$")


def write_outputs(outputs, path=None):
    """Write outputs (a dict) as the termination message; only rank 0 should call this."""
    clean = {}
    for k, v in outputs.items():
        if not _KEY.match(k):
            raise ValueError(f"output key {k!r} must match [A-Za-z0-9_-]{{1,63}}")
        clean[k] = v if isinstance(v, str) else json.dumps(v)
    data = json.dumps(clean, sort_keys=True, separators=(",", ":"))
    if len(data.encode()) > MAX_BYTES:
        raise ValueError(f"outputs are {len(data.encode())} bytes; the termination message keeps {MAX_BYTES}")
    with open(path or TERMINATION_LOG, "w") as f:
        f.write(data)
    print("outputs:", data, flush=True)
    return clean


def is_rank_zero():
    return os.environ.get("RANK", os.environ.get("JOB_COMPLETION_INDEX", "0")) == "0"
