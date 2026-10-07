"""Dependency-free framework timing adapter. Durations use monotonic time.

Wrap a whole step with ``measure('step')`` and its synchronous phases with
``measure('data')`` / ``measure('collective')``. For asynchronous CUDA/NCCL work,
synchronize or use framework events before reporting elapsed time. No GPU utilization
is inferred from wall time. Call snapshot after a complete measurement window.
"""
from contextlib import contextmanager
from datetime import datetime, timezone
import math
from statistics import mean
import time


class TrainingTelemetry:
    def __init__(self, rank=0, source="framework-timer", max_samples=10000):
        if not isinstance(rank, int) or isinstance(rank, bool) or not 0 <= rank <= 65535:
            raise ValueError("invalid rank")
        if not 1 <= max_samples <= 10000:
            raise ValueError("max_samples must be 1-10000")
        self.rank, self.source, self.max_samples = rank, source, max_samples
        self.reset()

    def reset(self):
        self.started = time.monotonic()
        self.samples = {"step": [], "data": [], "collective": []}

    @contextmanager
    def measure(self, phase):
        if phase not in self.samples:
            raise ValueError("phase must be step, data or collective")
        start = time.monotonic()
        try:
            yield
        except BaseException:
            raise  # aborted phases are not counted as completed measurements
        else:
            self.record(phase, time.monotonic() - start)

    def record(self, phase, seconds):
        if phase not in self.samples or isinstance(seconds, bool) or not math.isfinite(seconds) or seconds < 0:
            raise ValueError("invalid timing observation")
        if len(self.samples[phase]) >= self.max_samples:
            raise ValueError("measurement window is full; snapshot and reset")
        self.samples[phase].append(seconds)

    def snapshot(self, gpu_utilization=None):
        if not self.samples["step"] or mean(self.samples["step"]) <= 0:
            raise ValueError("no completed positive-duration steps")
        if gpu_utilization is not None and (isinstance(gpu_utilization, bool)
                                           or not math.isfinite(gpu_utilization) or not 0 <= gpu_utilization <= 100):
            raise ValueError("invalid GPU utilization")
        output = {"rank": self.rank, "source": self.source, "observedAt": datetime.now(timezone.utc).isoformat(),
                  "windowSeconds": max(1e-9, time.monotonic() - self.started),
                  "stepSeconds": mean(self.samples["step"]), "gpuUtilization": gpu_utilization}
        for phase, key in (("data", "dataWaitSeconds"), ("collective", "collectiveSeconds")):
            # Sum phase calls per completed step; multiple data/collective calls are allowed in a step.
            output[key] = sum(self.samples[phase]) / len(self.samples["step"]) if self.samples[phase] else None
        return output
