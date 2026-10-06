"""Gryvia Python SDK client."""

from __future__ import annotations

import os
from typing import Any

import httpx

from gryvia.costs import Costs
from gryvia.exceptions import (
    AuthenticationError,
    AuthorizationError,
    ConfigurationError,
    NotFoundError,
    RateLimitError,
    ServerError,
    GryviaError,
    ValidationError,
)
from gryvia.jobs import Jobs
from gryvia.intelligence import Intelligence
from gryvia.metrics import Metrics
from gryvia.nodes import Nodes
from gryvia.quotas import Quotas


class Gryvia:
    """Client for the Gryvia API.

    The client can be configured explicitly or via environment variables:

    * ``GRYVIA_API_URL`` -- base URL of the API gateway
    * ``GRYVIA_TOKEN``   -- Bearer token for authentication

    Args:
        api_url: Base URL of the Gryvia API gateway
            (e.g. ``"https://gryvia.example.com"``).
            Falls back to the ``GRYVIA_API_URL`` env var.
        token: Bearer token for authentication.
            Falls back to the ``GRYVIA_TOKEN`` env var.
        timeout: Default request timeout in seconds (default 30).
        verify_ssl: Whether to verify TLS certificates (default True).

    Example::

        from gryvia import Gryvia

        async with Gryvia(api_url="https://tr.example.com", token="secret") as tr:
            stats = await tr.metrics.cluster_stats()
            print(stats.total_gpus)

    Raises:
        ConfigurationError: If *api_url* or *token* cannot be determined.
    """

    def __init__(
        self,
        api_url: str | None = None,
        token: str | None = None,
        *,
        timeout: float = 30.0,
        verify_ssl: bool = True,
    ) -> None:
        self._api_url = (api_url or os.environ.get("GRYVIA_API_URL", "")).rstrip("/")
        self._token = token or os.environ.get("GRYVIA_TOKEN", "")

        if not self._api_url:
            raise ConfigurationError(
                "api_url is required. Pass it directly or set GRYVIA_API_URL."
            )
        if not self._token:
            raise ConfigurationError(
                "token is required. Pass it directly or set GRYVIA_TOKEN."
            )

        self._http = httpx.AsyncClient(
            base_url=self._api_url,
            timeout=httpx.Timeout(timeout),
            verify=verify_ssl,
        )

        # Sub-resource managers
        self.jobs = Jobs(self)
        self.quotas = Quotas(self)
        self.nodes = Nodes(self)
        self.metrics = Metrics(self)
        self.costs = Costs(self)
        self.intelligence = Intelligence(self)

    # ------------------------------------------------------------------
    # Context manager
    # ------------------------------------------------------------------

    async def __aenter__(self) -> Gryvia:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()

    async def close(self) -> None:
        """Close the underlying HTTP client."""
        await self._http.aclose()

    # ------------------------------------------------------------------
    # Health
    # ------------------------------------------------------------------

    async def health(self) -> dict[str, str]:
        """Check API gateway health (unauthenticated).

        Returns:
            A dict like ``{"status": "ok"}``.
        """
        resp = await self._http.get("/health")
        resp.raise_for_status()
        return resp.json()

    # ------------------------------------------------------------------
    # Internal HTTP helpers
    # ------------------------------------------------------------------

    def _auth_headers(self) -> dict[str, str]:
        """Return the authorization header dict."""
        return {"Authorization": f"Bearer {self._token}"}

    def _url(self, path: str) -> str:
        """Build a full URL for a given API path."""
        return f"{self._api_url}{path}"

    async def _get(
        self, path: str, *, params: dict[str, Any] | None = None
    ) -> Any:
        """Perform an authenticated GET request and return parsed JSON."""
        resp = await self._http.get(
            path, params=params, headers=self._auth_headers()
        )
        self._raise_for_status(resp, path)
        return resp.json()

    async def _post(
        self, path: str, *, json: Any = None
    ) -> Any:
        """Perform an authenticated POST request and return parsed JSON."""
        resp = await self._http.post(
            path, json=json, headers=self._auth_headers()
        )
        self._raise_for_status(resp, path)
        return resp.json()

    async def _delete(self, path: str) -> Any:
        """Perform an authenticated DELETE request and return parsed JSON."""
        resp = await self._http.delete(path, headers=self._auth_headers())
        self._raise_for_status(resp, path)
        return resp.json()

    @staticmethod
    def _raise_for_status(resp: httpx.Response, path: str) -> None:
        """Translate HTTP error responses into SDK exceptions."""
        if resp.is_success:
            return

        detail = ""
        try:
            body = resp.json()
            detail = body.get("detail", "")
        except Exception:
            detail = resp.text

        status = resp.status_code

        if status == 400:
            raise ValidationError(detail or "Bad request", detail=detail)
        if status == 401:
            raise AuthenticationError(detail=detail)
        if status == 403:
            raise AuthorizationError(detail=detail)
        if status == 404:
            # Try to extract resource type/name from path
            parts = path.strip("/").split("/")
            resource_type = parts[-2] if len(parts) >= 2 else "Resource"
            resource_name = parts[-1] if parts else "unknown"
            raise NotFoundError(resource_type, resource_name)
        if status == 429:
            raise RateLimitError(detail=detail)
        if status >= 500:
            raise ServerError(
                message=detail or "Internal server error",
                status_code=status,
                detail=detail,
            )

        raise GryviaError(
            f"HTTP {status}: {detail}",
            status_code=status,
            detail=detail,
        )
