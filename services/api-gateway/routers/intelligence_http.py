"""Bound analysis payload size before Pydantic parses it."""

from fastapi import HTTPException
from fastapi.routing import APIRoute


class BoundedRoute(APIRoute):
    def get_route_handler(self):
        original = super().get_route_handler()

        async def bounded(request):
            if request.method in ("POST", "PUT", "PATCH"):
                chunks, size = [], 0
                async for chunk in request.stream():
                    size += len(chunk)
                    if size > 524288:
                        raise HTTPException(status_code=413, detail="intelligence payload exceeds 512 KiB")
                    chunks.append(chunk)
                request._body = b"".join(chunks)
            return await original(request)

        return bounded
