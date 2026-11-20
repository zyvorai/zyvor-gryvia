# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(__file__)))

from routers.phases import bucket, count_phases, is_billable  # noqa: E402


@pytest.mark.parametrize("phase,expected", [
    ("Running", "running"), ("RUNNING", "running"), ("running", "running"),
    ("Pending", "pending"), ("queued", "pending"), ("Scheduling", "pending"),
    ("Succeeded", "completed"), ("Completed", "completed"), ("completed", "completed"),
    ("Failed", "failed"), ("FAILED", "failed"),
    ("Paused", "other"), ("", "other"), (None, "other"), (3, "other"),
])
def test_bucket(phase, expected):
    assert bucket(phase) == expected


def test_count_phases():
    assert count_phases(["Running", "running", "Succeeded", "Completed", "Queued", "Scheduling",
                         "Pending", "Failed", None, "Cancelled"]) == {
        "running": 2, "pending": 3, "completed": 2, "failed": 1, "other": 2}


@pytest.mark.parametrize("phase,expected", [
    ("Running", True), ("completed", True), ("Succeeded", True), ("Pending", False), ("Failed", False), (None, False)])
def test_is_billable(phase, expected):
    assert is_billable(phase) is expected
