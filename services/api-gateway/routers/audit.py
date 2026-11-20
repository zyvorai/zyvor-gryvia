# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Audit trail for the gateway: who changed what, and who was refused.

Every state-changing request (POST, PUT, PATCH, DELETE) is recorded after it completes, including
failed logins and refused requests (401/403), which are the interesting ones. An entry holds the time, a
request id, the caller (auth method, role, tenant, OIDC subject), the method, the route template and path,
the status and outcome, the client address and the duration. It never holds the Authorization header, a
query string or a request body.

Where entries go:
  * the ``gryvia.audit`` logger, one JSON object per line (this is the durable record: ship it with your
    log collector);
  * optionally a JSON-lines file (GRYVIA_AUDIT_FILE), appended with 0600 permissions;
  * optionally a SQLite database (GRYVIA_AUDIT_DB, a path on a persistent volume): durable, searchable by
    time, actor, path and status, and exportable as CSV. GRYVIA_AUDIT_RETENTION_DAYS (default 0 = keep
    everything) prunes older rows. SQLite is one file with one writer: use it with a single gateway
    replica, or per-replica files; it is not a shared store;
  * a bounded in-memory buffer (GRYVIA_AUDIT_BUFFER entries, default 1000) behind ``GET /api/audit`` for
    the provider administrator. The buffer is per gateway replica and is lost on restart; use the log or
    file for anything that must survive.

The client address is the socket peer, not X-Forwarded-For (which a caller can forge). Behind a proxy it is
the proxy's address.
"""
import json
import logging
import csv
import io
import os
import sqlite3
import threading
import time
import uuid
from collections import deque
from datetime import datetime, timedelta, timezone
from typing import Any, Deque, Dict, List, Optional

from fastapi import APIRouter, Depends, FastAPI, Query, Request
from fastapi.responses import Response

from .common import Deps, require_admin
from . import observability

audit_logger = logging.getLogger("gryvia.audit")

MUTATING = frozenset({"POST", "PUT", "PATCH", "DELETE"})
OUTCOMES = ("success", "denied", "error")


def _buffer_size() -> int:
    try:
        n = int(os.environ.get("GRYVIA_AUDIT_BUFFER", "1000"))
    except ValueError:
        return 1000
    return max(10, min(n, 100_000))


_COLUMNS = ("time", "id", "auth", "role", "tenant", "subject", "method", "route", "path", "status", "outcome",
            "ip", "durationMs")
_PRUNE_EVERY = 500  # inserts between retention sweeps


def _retention_days() -> int:
    try:
        return max(0, int(os.environ.get("GRYVIA_AUDIT_RETENTION_DAYS", "0")))
    except ValueError:
        return 0


def _row(e: Dict[str, Any]) -> tuple:
    a = e.get("actor") or {}
    return (e["time"], e["id"], a.get("auth", ""), a.get("role", ""), a.get("tenant", ""), a.get("subject", ""),
            e["method"], e["route"], e["path"], e["status"], e["outcome"], e.get("ip", ""), e["durationMs"])


def _entry(r: tuple) -> Dict[str, Any]:
    t = dict(zip(_COLUMNS, r))
    return {"time": t["time"], "id": t["id"],
            "actor": {k: t[k] for k in ("auth", "role", "tenant", "subject")},
            "method": t["method"], "route": t["route"], "path": t["path"], "status": t["status"],
            "outcome": t["outcome"], "ip": t["ip"], "durationMs": t["durationMs"]}


class AuditLog:
    """Thread-safe bounded buffer plus the log, file and SQLite sinks."""

    def __init__(self, size: Optional[int] = None, path: Optional[str] = None, db: Optional[str] = None,
                 retention_days: Optional[int] = None) -> None:
        self._lock = threading.Lock()
        self._entries: Deque[Dict[str, Any]] = deque(maxlen=size or _buffer_size())
        self._path = path if path is not None else os.environ.get("GRYVIA_AUDIT_FILE", "") or None
        self._retention = _retention_days() if retention_days is None else retention_days
        self._inserts = 0
        self._db: Optional[sqlite3.Connection] = None
        db_path = db if db is not None else os.environ.get("GRYVIA_AUDIT_DB", "") or None
        if db_path:
            try:
                fd = os.open(db_path, os.O_RDWR | os.O_CREAT, 0o600)
                os.close(fd)
                self._db = sqlite3.connect(db_path, check_same_thread=False)
                self._db.execute("PRAGMA journal_mode=WAL")
                self._db.execute(f"CREATE TABLE IF NOT EXISTS audit ({', '.join(_COLUMNS)})")
                self._db.execute("CREATE INDEX IF NOT EXISTS audit_time ON audit(time)")
                self._db.commit()
                self._prune()
            except (OSError, sqlite3.Error) as exc:  # fall back to the buffer rather than refuse to start
                audit_logger.error("audit database %s unusable: %s", db_path, exc)
                self._db = None

    def _prune(self) -> None:
        if self._db is None or not self._retention:
            return
        cutoff = (datetime.now(timezone.utc) - timedelta(days=self._retention)).isoformat(timespec="milliseconds")
        self._db.execute("DELETE FROM audit WHERE time < ?", (cutoff.replace("+00:00", "Z"),))
        self._db.commit()

    def record(self, entry: Dict[str, Any]) -> None:
        line = json.dumps(entry, separators=(",", ":"), sort_keys=True)
        with self._lock:
            self._entries.append(entry)
            if self._db is not None:
                try:
                    self._db.execute(f"INSERT INTO audit VALUES ({','.join('?' * len(_COLUMNS))})", _row(entry))
                    self._db.commit()
                    self._inserts += 1
                    if self._inserts % _PRUNE_EVERY == 0:
                        self._prune()
                except sqlite3.Error as exc:  # never fail a request because the audit database is unwritable
                    audit_logger.error("audit database write failed: %s", exc)
        audit_logger.info("%s", line)
        if self._path:
            try:
                fd = os.open(self._path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
                with os.fdopen(fd, "a", encoding="utf-8") as f:
                    f.write(line + "\n")
            except OSError as exc:  # never fail a request because the audit file is unwritable
                audit_logger.error("audit file %s not writable: %s", self._path, exc)

    @property
    def durable(self) -> bool:
        return self._db is not None

    def query(self, limit: int = 100, outcome: Optional[str] = None, method: Optional[str] = None,
              since: Optional[str] = None, until: Optional[str] = None, actor: Optional[str] = None,
              path: Optional[str] = None, status: Optional[int] = None) -> List[Dict[str, Any]]:
        """Newest first. since/until are ISO timestamps; actor matches subject or tenant, path is a substring."""
        if self._db is not None:
            where, args = [], []  # type: List[str], List[Any]
            for clause, value in (("outcome = ?", outcome), ("method = ?", method.upper() if method else None),
                                  ("time >= ?", since), ("time <= ?", until), ("status = ?", status)):
                if value is not None:
                    where.append(clause)
                    args.append(value)
            if actor:
                where.append("(subject = ? OR tenant = ?)")
                args += [actor, actor]
            if path:
                where.append("instr(path, ?) > 0")
                args.append(path)
            sql = f"SELECT {', '.join(_COLUMNS)} FROM audit" + (" WHERE " + " AND ".join(where) if where else "")
            with self._lock:
                rows = self._db.execute(sql + " ORDER BY time DESC LIMIT ?", (*args, limit)).fetchall()
            return [_entry(r) for r in rows]
        with self._lock:
            items = list(self._entries)
        out: List[Dict[str, Any]] = []
        for e in reversed(items):
            if outcome and e["outcome"] != outcome:
                continue
            if method and e["method"] != method.upper():
                continue
            if since and e["time"] < since or until and e["time"] > until:
                continue
            if actor and actor not in (e["actor"]["subject"], e["actor"]["tenant"]):
                continue
            if path and path not in e["path"]:
                continue
            if status is not None and e["status"] != status:
                continue
            out.append(e)
            if len(out) >= limit:
                break
        return out


def outcome_of(status: int) -> str:
    if status in (401, 403):
        return "denied"
    return "success" if status < 400 else "error"


def _actor(request: Request) -> Dict[str, Any]:
    st = request.state
    claims = getattr(st, "user_claims", None) or {}
    subject = ""
    if isinstance(claims, dict):
        subject = str(claims.get("email") or claims.get("sub") or "")[:200]
    return {
        "auth": getattr(st, "auth_method", None) or "none",
        "role": getattr(st, "role", None) or "none",
        "tenant": getattr(st, "tenant", None) or "",
        "subject": subject,
    }


def _route(request: Request) -> str:
    route = request.scope.get("route")
    path = getattr(route, "path", None)
    return path if isinstance(path, str) else "unmatched"


def build_entry(request: Request, status: int, started: float) -> Dict[str, Any]:
    rid = request.headers.get("x-request-id", "")
    if not (0 < len(rid) <= 64 and rid.isprintable()):
        rid = uuid.uuid4().hex
    return {
        "time": datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"),
        "id": rid,
        "actor": _actor(request),
        "method": request.method,
        "route": _route(request),
        "path": request.url.path[:300],
        "status": status,
        "outcome": outcome_of(status),
        "ip": request.client.host if request.client else "",
        "durationMs": round((time.perf_counter() - started) * 1000, 1),
    }


# One log per process; tests replace it through install(app, log).
LOG = AuditLog()


def install(app: FastAPI, log: Optional[AuditLog] = None) -> AuditLog:
    """Attach the audit middleware to app and return the log it writes to."""
    sink = log or LOG
    app.state.audit_log = sink  # read per request, so tests (or an operator hook) can swap it

    @app.middleware("http")
    async def _audit(request: Request, call_next):
        if request.method not in MUTATING:
            return await call_next(request)
        started = time.perf_counter()
        status = 500
        try:
            response = await call_next(request)
            status = response.status_code
            return response
        finally:
            entry = build_entry(request, status, started)
            request.app.state.audit_log.record(entry)
            observability.record_audit(entry["method"], entry["outcome"])

    return sink


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    def _filters(outcome, method, since, until, actor, path, status):
        return dict(outcome=outcome, method=method, since=since, until=until, actor=actor, path=path, status=status)

    @router.get("/api/audit")
    @deps.limiter.limit("30/minute")
    async def list_audit(
        request: Request,
        limit: int = Query(100, ge=1, le=1000),
        outcome: Optional[str] = Query(None, pattern="^(success|denied|error)$"),
        method: Optional[str] = Query(None, pattern="^(?i:POST|PUT|PATCH|DELETE)$"),
        since: Optional[str] = Query(None, max_length=40),
        until: Optional[str] = Query(None, max_length=40),
        actor: Optional[str] = Query(None, max_length=200),
        path: Optional[str] = Query(None, max_length=300),
        status: Optional[int] = Query(None, ge=100, le=599),
        _=Depends(deps.verify_auth),
        __=Depends(require_admin),
    ):
        log: AuditLog = request.app.state.audit_log
        note = ("stored in the audit database" if log.durable else
                "per-replica buffer, lost on restart; the gryvia.audit log is the durable record")
        return {"items": log.query(limit, **_filters(outcome, method, since, until, actor, path, status)),
                "durable": log.durable, "note": note}

    @router.get("/api/audit/export.csv")
    @deps.limiter.limit("5/minute")
    async def export_audit(
        request: Request,
        limit: int = Query(10000, ge=1, le=100000),
        outcome: Optional[str] = Query(None, pattern="^(success|denied|error)$"),
        method: Optional[str] = Query(None, pattern="^(?i:POST|PUT|PATCH|DELETE)$"),
        since: Optional[str] = Query(None, max_length=40),
        until: Optional[str] = Query(None, max_length=40),
        actor: Optional[str] = Query(None, max_length=200),
        path: Optional[str] = Query(None, max_length=300),
        status: Optional[int] = Query(None, ge=100, le=599),
        _=Depends(deps.verify_auth),
        __=Depends(require_admin),
    ):
        log: AuditLog = request.app.state.audit_log
        buf = io.StringIO()
        w = csv.writer(buf)
        w.writerow(_COLUMNS)
        for e in log.query(limit, **_filters(outcome, method, since, until, actor, path, status)):
            # Spreadsheet formula injection: the path and subject are caller-controlled.
            w.writerow(["'" + str(v) if isinstance(v, str) and v[:1] in "=+-@\t\r" else v for v in _row(e)])
        return Response(buf.getvalue(), media_type="text/csv",
                        headers={"Content-Disposition": 'attachment; filename="audit.csv"'})

    return router
