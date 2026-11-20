# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Relays an upstream OpenAI-style chat completion stream (server-sent events) to the caller."""
import json
from typing import Any, Callable, Dict, Optional

import httpx
from fastapi import HTTPException
from fastapi.responses import JSONResponse, StreamingResponse


async def relay(url: str, body: Dict[str, Any], *, timeout: float, label: str,
                error: Callable[[int, Any], HTTPException], headers: Optional[Dict[str, str]] = None,
                transport: Optional[httpx.AsyncBaseTransport] = None) -> Any:
    """Opens the upstream event stream and relays it. An error status before the stream starts is raised through
    ``error(status, json_body)``; a JSON reply to a streamed request is passed through."""
    client = httpx.AsyncClient(timeout=timeout, follow_redirects=False, trust_env=False, transport=transport)
    try:
        resp = await client.send(client.build_request("POST", url, json=body, headers=headers), stream=True)
    except httpx.HTTPError as exc:
        await client.aclose()
        raise HTTPException(status_code=502, detail=f"{label} is unreachable: {type(exc).__name__}")
    if resp.status_code != 200 or not resp.headers.get("content-type", "").startswith("text/event-stream"):
        try:
            raw = await resp.aread()
        finally:
            await resp.aclose()
            await client.aclose()
        try:
            data = json.loads(raw) if raw else {}
        except ValueError:
            data = {"error": {"message": raw[:300].decode(errors="replace")}}
        if resp.status_code != 200:
            raise error(resp.status_code, data)
        return JSONResponse(data)

    async def events():
        try:
            async for chunk in resp.aiter_bytes():
                yield chunk
        except httpx.HTTPError as exc:
            err = {"error": {"message": f"{label} stream interrupted: {type(exc).__name__}"}}
            yield f"\n\ndata: {json.dumps(err)}\n\ndata: [DONE]\n\n".encode()
        finally:
            await resp.aclose()
            await client.aclose()

    return StreamingResponse(events(), media_type="text/event-stream",
                             headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"})
