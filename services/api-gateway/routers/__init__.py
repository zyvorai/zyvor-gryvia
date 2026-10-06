"""Route modules for the Gryvia API gateway."""
import importlib
import logging

from fastapi import FastAPI

from .common import Deps

logger = logging.getLogger(__name__)

# Modules under routers/ that expose build_router(deps). A missing module is skipped
# (with a warning) so the gateway still starts; tests cover each module.
MODULES = ["network", "security", "workspaces", "jobs", "models", "inference", "workflows", "model_watches", "datasets", "llm", "vector_indexes", "agents", "jobhooks", "tuners", "ai", "catalog", "tenants", "usage", "invoices", "reservations", "budgets", "flight", "netusage", "audit", "experiments", "federation", "sovereign", "lineage", "markings", "copilot", "data_catalog"]


def register_routers(app: FastAPI, deps: Deps) -> None:
    for name in [*MODULES, "intelligence", "intelligence_actions"]:
        try:
            mod = importlib.import_module(f"{__name__}.{name}")
        except ModuleNotFoundError as exc:
            if exc.name != f"{__name__}.{name}":
                raise
            logger.warning("router module %s not found; its routes are unavailable", name)
            continue
        app.include_router(mod.build_router(deps))
        logger.info("registered routes from routers.%s", name)
