# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Audit trail through the real gateway (main.py): what is recorded, for whom, and what never is."""
import json
import logging

import pytest

from tests.test_oidc import API_KEY, claims, env, keys  # noqa: F401  (fixtures)
from tests.test_roles import gw  # noqa: F401  (fixture)


def entries(g, **params):
    r = g.get("/api/audit", params=params)
    assert r.status_code == 200, r.text
    return r.json()["items"]


def test_mutating_request_is_recorded_with_actor_route_and_status(gw):
    r = gw.call("DELETE", "/api/jobs/does-not-exist")
    got = entries(gw, method="DELETE")
    assert got and got[0]["method"] == "DELETE"
    e = got[0]
    assert e["route"] == "/api/jobs/{name}" and e["path"] == "/api/jobs/does-not-exist"
    assert e["status"] == r.status_code and e["actor"]["role"] == "admin" and e["actor"]["auth"] == "api_key"
    assert e["id"] and e["time"].endswith("Z") and e["durationMs"] >= 0


def test_reads_are_not_recorded(gw):
    gw.get("/api/nodes")
    gw.get("/api/jobs")
    assert [e for e in entries(gw) if e["method"] == "GET"] == []


def test_refused_requests_are_recorded_as_denied(gw):
    bad = gw.client.post("/api/jobs", headers={"Authorization": "Bearer wrong"}, json={})
    assert bad.status_code in (401, 403)
    none = gw.client.post("/api/jobs", json={})
    assert none.status_code == 401
    denied = entries(gw, outcome="denied")
    assert len(denied) >= 2 and all(e["actor"]["role"] == "none" for e in denied[:2])


def test_failed_login_is_recorded(gw):
    r = gw.client.post("/api/auth/login", json={"username": "admin", "password": "nope"})
    assert r.status_code in (401, 403)
    e = [x for x in entries(gw) if x["route"] == "/api/auth/login"][0]
    assert e["outcome"] == "denied" and e["actor"]["auth"] == "none"


def test_oidc_tenant_actor_is_recorded_and_cannot_read_the_trail(gw):
    tok = gw.tenant_token(org="alpha", email="dev@alpha.example")
    gw.call("DELETE", "/api/jobs/a-job", tok)
    mine = [e for e in entries(gw) if e["actor"]["auth"] == "oidc"]
    assert mine and mine[0]["actor"]["tenant"] == "alpha" and mine[0]["actor"]["role"] == "tenant"
    assert gw.get("/api/audit", tok).status_code == 403


def test_secrets_never_reach_the_trail(gw, caplog):
    caplog.set_level(logging.INFO, logger="gryvia.audit")
    gw.call("DELETE", "/api/jobs/x?token=hunter2", params=None)
    blob = json.dumps(entries(gw)) + caplog.text
    assert API_KEY not in blob and "hunter2" not in blob and "Bearer" not in blob


def test_each_entry_is_one_json_log_line(gw, caplog):
    caplog.set_level(logging.INFO, logger="gryvia.audit")
    gw.call("DELETE", "/api/jobs/zzz")
    lines = [r.getMessage() for r in caplog.records if r.name == "gryvia.audit"]
    assert lines and json.loads(lines[-1])["path"] == "/api/jobs/zzz"


def test_buffer_is_bounded_and_filters_work(gw):
    from routers.audit import AuditLog

    log = AuditLog(size=10)
    gw.main_.app.state.audit_log = log
    for i in range(25):
        gw.call("DELETE", f"/api/jobs/j{i}")
    got = entries(gw, limit=1000)
    assert len(got) == 10 and got[0]["path"] == "/api/jobs/j24"  # newest first, oldest dropped
    assert gw.get("/api/audit", params={"outcome": "bogus"}).status_code == 422


def test_audit_file_sink_is_0600_jsonl(tmp_path):
    import os
    from routers.audit import AuditLog

    p = tmp_path / "audit.jsonl"
    log = AuditLog(size=10, path=str(p))
    log.record({"outcome": "success", "method": "POST", "path": "/x"})
    assert json.loads(p.read_text().splitlines()[0])["path"] == "/x"
    assert (os.stat(p).st_mode & 0o777) == 0o600


def test_unwritable_audit_file_does_not_fail_requests(tmp_path):
    from routers.audit import AuditLog

    log = AuditLog(size=10, path=str(tmp_path / "no" / "such" / "dir" / "a.jsonl"))
    log.record({"outcome": "success", "method": "POST", "path": "/x"})  # must not raise
    assert len(log.query()) == 1


def test_audit_counter_is_exported(gw):
    from routers import observability

    before = observability.AUDIT_EVENTS.labels("DELETE", "error")._value.get() + observability.AUDIT_EVENTS.labels("DELETE", "success")._value.get()
    gw.call("DELETE", "/api/jobs/metric-probe")
    after = observability.AUDIT_EVENTS.labels("DELETE", "error")._value.get() + observability.AUDIT_EVENTS.labels("DELETE", "success")._value.get()
    assert after == before + 1


# -- SQLite sink --------------------------------------------------------------------------------

def _entry(**kw):
    e = {"time": "2026-01-01T00:00:00.000Z", "id": "r1", "actor": {"auth": "api_key", "role": "admin", "tenant": "alpha", "subject": "a@x"},
         "method": "POST", "route": "/api/jobs", "path": "/api/jobs", "status": 201, "outcome": "success",
         "ip": "1.2.3.4", "durationMs": 1.5}
    e.update(kw)
    return e


def test_sqlite_survives_restart_and_filters(tmp_path):
    from routers.audit import AuditLog
    db = str(tmp_path / "audit.db")
    log = AuditLog(db=db)
    log.record(_entry(id="a", time="2026-01-01T00:00:00.000Z"))
    log.record(_entry(id="b", time="2026-01-02T00:00:00.000Z", method="DELETE", path="/api/jobs/x", status=403,
                      outcome="denied", actor={"auth": "oidc", "role": "tenant", "tenant": "beta", "subject": "b@x"}))
    again = AuditLog(db=db)  # a new process reading the same file
    assert again.durable and [e["id"] for e in again.query()] == ["b", "a"]
    assert [e["id"] for e in again.query(outcome="denied")] == ["b"]
    assert [e["id"] for e in again.query(actor="beta")] == ["b"]
    assert [e["id"] for e in again.query(path="jobs/x")] == ["b"]
    assert [e["id"] for e in again.query(since="2026-01-02T00:00:00.000Z")] == ["b"]
    assert [e["id"] for e in again.query(status=201)] == ["a"]
    assert again.query()[0]["actor"]["subject"] == "b@x"


def test_sqlite_retention_prunes_old_rows(tmp_path):
    from routers.audit import AuditLog
    db = str(tmp_path / "audit.db")
    AuditLog(db=db).record(_entry(time="2020-01-01T00:00:00.000Z"))
    assert AuditLog(db=db, retention_days=30).query() == []


def test_unusable_db_falls_back_to_buffer(tmp_path):
    from routers.audit import AuditLog
    log = AuditLog(db=str(tmp_path / "no" / "such" / "dir" / "a.db"))
    log.record(_entry())
    assert not log.durable and len(log.query()) == 1


def test_csv_export_neutralises_formulas(gw):
    gw.client.app.state.audit_log.record(_entry(actor={"auth": "oidc", "role": "tenant", "tenant": "t", "subject": "=HYPERLINK(\"x\")"}))
    r = gw.get("/api/audit/export.csv")
    assert r.status_code == 200 and r.headers["content-type"].startswith("text/csv")
    assert "'=HYPERLINK" in r.text and ",=HYPERLINK" not in r.text
