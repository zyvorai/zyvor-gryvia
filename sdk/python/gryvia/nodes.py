# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Node management for Gryvia."""

from __future__ import annotations

from typing import TYPE_CHECKING

from gryvia.models import Node, NodeHealthResponse, NodeListResponse

if TYPE_CHECKING:
    from gryvia.client import Gryvia


class Nodes:
    """Manage GryviaGPUNode resources.

    This class is not instantiated directly -- use ``client.nodes`` instead.
    """

    def __init__(self, client: Gryvia) -> None:
        self._client = client

    async def list(self, *, limit: int = 100, offset: int = 0) -> NodeListResponse:
        """List all GPU nodes with pagination.

        Args:
            limit: Maximum number of nodes to return (1-1000).
            offset: Number of nodes to skip.

        Returns:
            A ``NodeListResponse`` containing the paginated node list.
        """
        data = await self._client._get(
            "/api/nodes", params={"limit": limit, "offset": offset}
        )
        return NodeListResponse.model_validate(data)

    async def get(self, name: str) -> Node:
        """Get a single GPU node by name.

        Args:
            name: The node name.

        Returns:
            The ``Node`` resource.

        Raises:
            NotFoundError: If the node does not exist.
        """
        data = await self._client._get(f"/api/nodes/{name}")
        return Node.model_validate(data)

    async def health(self, *, limit: int = 100, offset: int = 0) -> NodeHealthResponse:
        """Get GPU node health status.

        Returns per-node health information including temperature alerts,
        RDMA status, and overall health assessment.

        Args:
            limit: Maximum entries to return (1-1000).
            offset: Number of entries to skip.

        Returns:
            A ``NodeHealthResponse`` with per-node health data.
        """
        data = await self._client._get(
            "/api/nodes/health", params={"limit": limit, "offset": offset}
        )
        return NodeHealthResponse.model_validate(data)
