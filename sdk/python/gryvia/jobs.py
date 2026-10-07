"""Job management for Gryvia."""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import TYPE_CHECKING, Any, AsyncIterator

import yaml

from gryvia.exceptions import TimeoutError, ValidationError
from gryvia.models import Job, JobListResponse, JobMetrics

if TYPE_CHECKING:
    from gryvia.client import Gryvia


class Jobs:
    """Manage GryviaAIJob resources.

    This class is not instantiated directly -- use ``client.jobs`` instead.
    """

    def __init__(self, client: Gryvia) -> None:
        self._client = client

    # ------------------------------------------------------------------
    # CRUD
    # ------------------------------------------------------------------

    async def list(self, *, limit: int = 100, offset: int = 0) -> JobListResponse:
        """List all jobs with pagination.

        Args:
            limit: Maximum number of jobs to return (1-1000).
            offset: Number of jobs to skip.

        Returns:
            A ``JobListResponse`` containing the paginated job list.
        """
        data = await self._client._get(
            "/api/jobs", params={"limit": limit, "offset": offset}
        )
        return JobListResponse.model_validate(data)

    async def preflight(self, job: dict[str, Any] | str | Path) -> dict[str, Any]:
        """Preview Kubernetes admission without creation or GPU reservation.

        Accepts the same manifest or YAML path as submit. Submit repeats the check only when the
        gateway enforces it (``enforcedOnCreate`` in the report).
        """
        return await self._client._post("/api/jobs/preflight", json=self._resolve_body(job))

    async def get(self, name: str) -> Job:
        """Get a single job by name.

        Args:
            name: The job name.

        Returns:
            The ``Job`` resource.

        Raises:
            NotFoundError: If the job does not exist.
        """
        data = await self._client._get(f"/api/jobs/{name}")
        return Job.model_validate(data)

    async def create(self, job: dict[str, Any] | str | Path) -> Job:
        """Submit a new job.

        Args:
            job: Either a dict describing the full GryviaAIJob manifest,
                 or a path (str or Path) to a YAML file containing one.

        Returns:
            The created ``Job`` resource as returned by the API.

        Raises:
            ValidationError: If the manifest is invalid.

        Example::

            # From a dict
            await client.jobs.create({
                "apiVersion": "gryvia.io/v1alpha1",
                "kind": "GryviaAIJob",
                "metadata": {"name": "my-training"},
                "spec": {
                    "image": "nvcr.io/nvidia/pytorch:24.01-py3",
                    "gpus": 4,
                    "gpuType": "A100-80G",
                    "framework": "pytorch",
                    "command": ["torchrun", "--nproc_per_node=4", "train.py"],
                },
            })

            # From a YAML file
            await client.jobs.create("jobs/my-training.yaml")
        """
        body = self._resolve_body(job)
        data = await self._client._post("/api/jobs", json=body)
        return Job.model_validate(data)

    async def delete(self, name: str) -> dict[str, str]:
        """Delete a job by name.

        Args:
            name: The job name.

        Returns:
            A dict with ``{"status": "deleted", "name": "<name>"}``.

        Raises:
            NotFoundError: If the job does not exist.
        """
        return await self._client._delete(f"/api/jobs/{name}")

    # ------------------------------------------------------------------
    # Extended operations
    # ------------------------------------------------------------------

    async def wait_for_completion(
        self,
        name: str,
        *,
        poll_interval: float = 5.0,
        timeout: float = 3600.0,
    ) -> Job:
        """Poll until a job reaches a terminal state.

        Terminal states are ``Completed`` and ``Failed``.

        Args:
            name: The job name.
            poll_interval: Seconds between polls (default 5).
            timeout: Maximum seconds to wait (default 3600 = 1 hour).

        Returns:
            The final ``Job`` resource.

        Raises:
            TimeoutError: If the job does not complete within *timeout*.
        """
        terminal_phases = {"Completed", "Failed"}
        elapsed = 0.0

        while elapsed < timeout:
            job = await self.get(name)
            if job.status.phase in terminal_phases:
                return job
            await asyncio.sleep(poll_interval)
            elapsed += poll_interval

        raise TimeoutError(
            f"Job '{name}' did not reach a terminal state within {timeout}s "
            f"(last phase: {job.status.phase})"
        )

    async def stream_logs(
        self,
        name: str,
        *,
        follow: bool = True,
        tail_lines: int | None = None,
    ) -> AsyncIterator[str]:
        """Stream logs from a running job.

        This connects to the ``/api/jobs/{name}/logs`` endpoint using
        server-sent events (SSE).  If the API gateway does not yet expose
        a logs endpoint, this method raises ``NotImplementedError`` with a
        descriptive message.

        Args:
            name: The job name.
            follow: If True, keep streaming until the job ends.
            tail_lines: Only return this many most-recent lines initially.

        Yields:
            Log lines as strings.
        """
        params: dict[str, Any] = {"follow": str(follow).lower()}
        if tail_lines is not None:
            params["tailLines"] = tail_lines

        url = self._client._url(f"/api/jobs/{name}/logs")
        async with self._client._http.stream(
            "GET",
            url,
            params=params,
            headers=self._client._auth_headers(),
            timeout=None,
        ) as response:
            if response.status_code == 404:
                raise NotImplementedError(
                    "The Gryvia API gateway does not expose a /api/jobs/{name}/logs "
                    "endpoint yet. Log streaming requires the API gateway to proxy "
                    "Kubernetes pod logs."
                )
            response.raise_for_status()
            async for line in response.aiter_lines():
                yield line

    # ------------------------------------------------------------------
    # Job metrics
    # ------------------------------------------------------------------

    async def metrics(self, *, time_range: str = "24h") -> JobMetrics:
        """Get aggregated job metrics.

        Args:
            time_range: Time range string (e.g. ``"1h"``, ``"24h"``, ``"7d"``).

        Returns:
            A ``JobMetrics`` object.
        """
        data = await self._client._get(
            "/api/metrics/jobs", params={"time_range": time_range}
        )
        return JobMetrics.model_validate(data)

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _resolve_body(job: dict[str, Any] | str | Path) -> dict[str, Any]:
        """Resolve a job argument to a dict body."""
        if isinstance(job, (str, Path)):
            path = Path(job)
            if not path.exists():
                raise ValidationError(f"YAML file not found: {path}")
            with open(path) as fh:
                body = yaml.safe_load(fh)
            if not isinstance(body, dict):
                raise ValidationError(f"YAML file did not produce a dict: {path}")
            return body
        if isinstance(job, dict):
            return job
        raise ValidationError(
            f"job must be a dict or a path to a YAML file, got {type(job).__name__}"
        )
