# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Quota management for Gryvia."""

from __future__ import annotations

from typing import TYPE_CHECKING

from gryvia.models import Quota, QuotaListResponse, QuotaUsageResponse

if TYPE_CHECKING:
    from gryvia.client import Gryvia


class Quotas:
    """Manage GryviaQuota resources.

    This class is not instantiated directly -- use ``client.quotas`` instead.
    """

    def __init__(self, client: Gryvia) -> None:
        self._client = client

    async def list(self, *, limit: int = 100, offset: int = 0) -> QuotaListResponse:
        """List all quotas with pagination.

        Args:
            limit: Maximum number of quotas to return (1-1000).
            offset: Number of quotas to skip.

        Returns:
            A ``QuotaListResponse`` containing the paginated quota list.
        """
        data = await self._client._get(
            "/api/quotas", params={"limit": limit, "offset": offset}
        )
        return QuotaListResponse.model_validate(data)

    async def get(self, name: str) -> Quota:
        """Get a single quota by name.

        Args:
            name: The quota name.

        Returns:
            The ``Quota`` resource.

        Raises:
            NotFoundError: If the quota does not exist.
        """
        data = await self._client._get(f"/api/quotas/{name}")
        return Quota.model_validate(data)

    async def usage(self, *, limit: int = 100, offset: int = 0) -> QuotaUsageResponse:
        """Get quota usage across all teams.

        This returns a summary view showing allocated vs. max GPUs,
        running/queued jobs, and budget information for every team.

        Args:
            limit: Maximum entries to return (1-1000).
            offset: Number of entries to skip.

        Returns:
            A ``QuotaUsageResponse`` with per-team usage data.
        """
        data = await self._client._get(
            "/api/quota/usage", params={"limit": limit, "offset": offset}
        )
        return QuotaUsageResponse.model_validate(data)
