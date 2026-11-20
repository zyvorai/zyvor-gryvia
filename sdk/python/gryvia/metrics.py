# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""GPU metrics for Gryvia."""

from __future__ import annotations

from typing import TYPE_CHECKING

from gryvia.models import ClusterStats, GPUMetricsResponse

if TYPE_CHECKING:
    from gryvia.client import Gryvia


class Metrics:
    """Access GPU and cluster metrics.

    This class is not instantiated directly -- use ``client.metrics`` instead.
    """

    def __init__(self, client: Gryvia) -> None:
        self._client = client

    async def cluster_stats(self) -> ClusterStats:
        """Get overall cluster statistics.

        Returns aggregated counts of GPUs, jobs, nodes, and the average
        GPU utilization percentage.

        Returns:
            A ``ClusterStats`` object.
        """
        data = await self._client._get("/api/cluster/stats")
        return ClusterStats.model_validate(data)

    async def gpu(self, *, time_range: str = "1h") -> GPUMetricsResponse:
        """Get GPU utilization metrics.

        Args:
            time_range: Time range for the query. Must be one of
                ``"1h"``, ``"6h"``, ``"24h"``, ``"7d"``, ``"30d"``.

        Returns:
            A ``GPUMetricsResponse`` containing per-GPU metrics.

        Raises:
            ValidationError: If *time_range* is not an allowed value.
        """
        data = await self._client._get(
            "/api/metrics/gpu", params={"time_range": time_range}
        )
        return GPUMetricsResponse.model_validate(data)
