# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Gryvia Python SDK -- interact with Gryvia GPU compute clusters.

Quick start::

    import asyncio
    from gryvia import Gryvia

    async def main():
        async with Gryvia(api_url="https://tr.example.com", token="secret") as tr:
            stats = await tr.metrics.cluster_stats()
            print(f"Total GPUs: {stats.total_gpus}")

            jobs = await tr.jobs.list()
            for job in jobs.items:
                print(f"  {job.metadata.name}: {job.status.phase}")

    asyncio.run(main())
"""

from gryvia.client import Gryvia
from gryvia.exceptions import (
    AuthenticationError,
    AuthorizationError,
    ConfigurationError,
    NotFoundError,
    RateLimitError,
    ServerError,
    GryviaError,
    TimeoutError,
    ValidationError,
)

__all__ = [
    "Gryvia",
    "GryviaError",
    "AuthenticationError",
    "AuthorizationError",
    "ConfigurationError",
    "NotFoundError",
    "RateLimitError",
    "ServerError",
    "TimeoutError",
    "ValidationError",
]

__version__ = "0.1.0"
