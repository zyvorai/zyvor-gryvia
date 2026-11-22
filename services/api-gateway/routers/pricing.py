# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""GPU price lookup: the GryviaGpuSku catalog is the source of truth.

The table below is used only when the cluster has no (enabled) SKU at all, so a fresh install still
reports estimates. Rates are per GPU-hour.
"""
from typing import Any, Dict

from .common import GROUP, VERSION, run

SKU_PLURAL = "gryviagpuskus"

FALLBACK_RATES: Dict[str, float] = {
    "H100": 8.00,
    "A100-80G": 4.00,
    "A100-40G": 3.50,
    "L40": 2.50,
    "V100": 2.00,
    "T4": 1.00,
}


def rates_from_skus(skus: list) -> Dict[str, float]:
    rates: Dict[str, float] = {}
    for sku in sorted(skus, key=lambda o: (o.get("metadata") or {}).get("name", "")):
        spec = sku.get("spec") or {}
        gpu_type, rate = spec.get("gpuType"), spec.get("hourlyRate")
        if spec.get("enabled", True) is False or not gpu_type or not isinstance(rate, (int, float)):
            continue
        rates.setdefault(gpu_type, float(rate))
    return rates


async def load_rates(custom: Any) -> Dict[str, float]:
    """gpuType -> hourly rate per GPU from enabled SKUs; the fallback table when none exist."""
    try:
        res = await run(custom.list_cluster_custom_object, group=GROUP, version=VERSION, plural=SKU_PLURAL)
        rates = rates_from_skus(res.get("items", []))
    except Exception:  # noqa: BLE001 - CRD missing / RBAC: price from the fallback table
        rates = {}
    return rates or dict(FALLBACK_RATES)
