# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Cost analysis for Gryvia."""

from __future__ import annotations

from typing import TYPE_CHECKING

from gryvia.models import CostMetrics

if TYPE_CHECKING:
    from gryvia.client import Gryvia


class Costs:
    """Access cost metrics and analysis.

    This class is not instantiated directly -- use ``client.costs`` instead.
    """

    def __init__(self, client: Gryvia) -> None:
        self._client = client

    async def get(self) -> CostMetrics:
        """Get cost metrics and analysis.

        Returns cost breakdowns by team and GPU type, including total
        cost for the current billing period.

        Returns:
            A ``CostMetrics`` object.
        """
        data = await self._client._get("/api/metrics/costs")
        return CostMetrics.model_validate(data)

    async def by_team(self) -> list[dict[str, float | str]]:
        """Get costs broken down by team.

        Convenience method that returns just the per-team cost list.

        Returns:
            A list of dicts with ``team`` and ``cost`` keys.
        """
        metrics = await self.get()
        return [{"team": t.team, "cost": t.cost} for t in metrics.by_team]

    async def by_gpu_type(self) -> list[dict[str, float | str]]:
        """Get costs broken down by GPU type.

        Convenience method that returns just the per-GPU-type cost list.

        Returns:
            A list of dicts with ``type``, ``cost``, and ``hours`` keys.
        """
        metrics = await self.get()
        return [
            {"type": g.type, "cost": g.cost, "hours": g.hours}
            for g in metrics.by_gpu_type
        ]

    async def total(self) -> float:
        """Get the total cost for the current period.

        Returns:
            Total cost as a float.
        """
        metrics = await self.get()
        return metrics.total_cost
