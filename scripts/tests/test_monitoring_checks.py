# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Tests for scripts/check-metrics-refs.py and scripts/check-monitoring-assets.py (run: python3 -m pytest scripts/tests)."""
import importlib.util
import json
import os
import shutil

import pytest

SCRIPTS = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ROOT = os.path.dirname(SCRIPTS)


def load(name, fname):
    spec = importlib.util.spec_from_file_location(name, os.path.join(SCRIPTS, fname))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


refs = load("check_metrics_refs", "check-metrics-refs.py")
assets = load("check_monitoring_assets", "check-monitoring-assets.py")


@pytest.mark.parametrize("expr,want", [
    ('sum by (namespace) (rate(gryvia_network_flow_bytes_total{namespace=~"$namespace"}[5m]))', ["gryvia_network_flow_bytes_total"]),
    ("histogram_quantile(0.99, sum by (le) (rate(x_seconds_bucket[5m])))", ["x_seconds_bucket"]),
    ("a / on (job) group_left b > 1e5 and c", ["a", "b", "c"]),
    ('label_values(gryvia_fabric_score_delta{namespace=~"$namespace"}, job)', ["gryvia_fabric_score_delta"]),
    ("count(m == 0) or vector(0)", ["m"]),
    ('m{a="}{", b!~"x|y"} offset 5m', ["m"]),
    ("DCGM_FI_DEV_FB_USED * 1024 * 1024", ["DCGM_FI_DEV_FB_USED"]),
])
def test_tokenizer(expr, want):
    assert refs.metric_names(expr) == want


def make_tree(tmp_path, dash_expr, rule_expr):
    root = tmp_path / "repo"
    (root / "collector").mkdir(parents=True)
    (root / "collector" / "m.go").write_text('Name: "gryvia_real_total",\n')
    (root / "collector" / "m_test.go").write_text('Name: "gryvia_test_only_total",\n')
    mon = root / "monitoring"
    (mon / "grafana-dashboards").mkdir(parents=True)
    (mon / "external-metrics.txt").write_text("up\nDCGM_FI_*  # comment\n")
    (mon / "grafana-dashboards" / "d.json").write_text(json.dumps({"panels": [{"targets": [{"expr": dash_expr}]}]}))
    (mon / "prometheus-rules.yaml").write_text(
        "groups:\n- name: g\n  rules:\n  - alert: A\n    expr: '%s'\n    for: 1m\n" % rule_expr)
    return root


def test_good_tree_passes(tmp_path):
    root = make_tree(tmp_path, "rate(gryvia_real_total[5m])", "DCGM_FI_DEV_GPU_UTIL > 90 and up == 1")
    unknown, n = refs.check(str(root), str(root / "monitoring"))
    assert unknown == {} and n == 2
    assert refs.main(["--root", str(root)]) == 0


def test_bad_fixture_fails_and_lists_unknown(tmp_path, capsys):
    root = make_tree(tmp_path, "gryvia_gpu_temperature_celsius > 85", "rate(gryvia_test_only_total[5m]) > 0")
    assert refs.main(["--root", str(root)]) == 1
    out = capsys.readouterr().out
    assert "gryvia_gpu_temperature_celsius" in out and "d.json" in out
    assert "gryvia_test_only_total" in out  # metrics that only tests define do not count as exported


def test_histogram_suffix_and_recording_rules_accepted(tmp_path):
    root = make_tree(tmp_path, "gryvia_real_total_bucket", "x:my_rule > 1")
    (root / "monitoring" / "rec.yaml").write_text("groups:\n- name: r\n  rules:\n  - record: x:my_rule\n    expr: up\n")
    unknown, _ = refs.check(str(root), str(root / "monitoring"))
    assert unknown == {}


def test_real_repo_assets_reference_only_known_metrics():
    unknown, n = refs.check(ROOT, os.path.join(ROOT, "monitoring"))
    assert unknown == {}, unknown
    assert n >= 10


def test_real_repo_assets_are_structurally_valid():
    rb = os.path.join(ROOT, "docs", "runbooks")
    assert assets.check_rules(os.path.join(ROOT, "monitoring", "prometheus-rules.yaml"), rb) == []
    assert assets.check_dashboards(os.path.join(ROOT, "monitoring", "grafana-dashboards")) == []


def test_asset_lint_catches_problems(tmp_path):
    rules = tmp_path / "r.yaml"
    rules.write_text("groups:\n- name: g\n  rules:\n  - alert: A\n    expr: up ==\n    labels: {severity: bad}\n")
    errs = assets.check_rules(str(rules), str(tmp_path))
    joined = "\n".join(errs)
    assert "`for`" in joined and "severity" in joined and "annotations.summary" in joined and "runbook_url" in joined
    d = tmp_path / "dash"
    d.mkdir()
    (d / "wrapped.json").write_text(json.dumps({"dashboard": {"panels": []}}))
    (d / "broken.json").write_text("{nope")
    (d / "noexpr.json").write_text(json.dumps({"uid": "u", "title": "t", "panels": [{"id": 1, "type": "timeseries", "title": "p", "targets": [], "gridPos": {}}],
                                               "templating": {}, "schemaVersion": 39}))
    joined = "\n".join(assets.check_dashboards(str(d)))
    assert "wrapped" in joined and "invalid JSON" in joined and "no target with an expr" in joined
