#!/usr/bin/env python3
"""Export a Gryvia cluster (jobs, GPU nodes, quotas) as a Zyvor Janus bundle.

Janus (https://github.com/zyvorai/janus) replays a cluster in a discrete-event simulator, so the same jobs can be
run under different scheduler policies without GPUs:

    ./scripts/export-janus-bundle.py --from-kubectl --out janus-bundle --profiles-dir ../janus/configs/profiles
    zyvor-janus run --zynera-bundle janus-bundle --scheduler bestfit --profiles-dir ../janus/configs/profiles

Janus reads Zynera's kinds (apiVersion zynera.ai/v1: FabricAIJob, FabricGpuNode, FabricQuota). Gryvia's GryviaGpuNode and
GryviaQuota specs have the same fields, so they are copied; a GryviaAIJob is translated:

    spec.gpus is GPUs PER POD in Gryvia      -> distributed.gpusPerNode when distributed, else gpus
    spec.migProfile + gpus                    -> mig {profile, count: gpus}
    spec.distributed.enabled, nodes > 1       -> annotations zynera.ai/gang-schedule and gang-size
    spec.model / spec.gpuType                 -> must have a calibrated profile in Janus' profiles dir

What the simulation means: Janus takes each job's runtime and GPU memory from its *calibrated model profile*, not from
the job. The profiles shipped with Janus are estimates. A job with no model, or a (model, GPU type) without a profile,
cannot be simulated and is skipped with a warning (or fails the export with --strict). Arrival times are not exported:
every job arrives at t=0.

Input: --from-kubectl (kubectl get ... -o json) or --from-dir DIR with one JSON/YAML file per kind (kubectl output,
`kind: List` or single objects). YAML input needs PyYAML; JSON does not. Output files are JSON, which is valid YAML.
"""
from __future__ import annotations

import argparse
import collections
import json
import os
import subprocess
import sys

API_VERSION = "zynera.ai/v1"
KINDS = {
    "jobs": ("gryviaaijobs", "GryviaAIJob"),
    "nodes": ("gryviagpunodes", "GryviaGpuNode"),
    "quotas": ("gryviaquotas", "GryviaQuota"),
    "tenants": ("gryviatenants", "GryviaTenant"),
}


def load_yaml_or_json(path):
    with open(path, encoding="utf-8") as f:
        text = f.read()
    if path.endswith(".json"):
        return [json.loads(text)]
    try:
        import yaml  # type: ignore
    except ImportError:
        sys.exit(f"{path}: YAML input needs PyYAML (pip install pyyaml); JSON input does not")
    return [d for d in yaml.safe_load_all(text) if d]


def flatten(docs):
    out = []
    for d in docs:
        if isinstance(d, dict) and d.get("kind") == "List":
            out.extend(d.get("items", []))
        elif isinstance(d, dict):
            out.append(d)
    return out


def read_dir(path):
    found = {k: [] for k in KINDS}
    for name in sorted(os.listdir(path)):
        if not name.endswith((".json", ".yaml", ".yml")):
            continue
        for obj in flatten(load_yaml_or_json(os.path.join(path, name))):
            for key, (_, kind) in KINDS.items():
                if obj.get("kind") == kind:
                    found[key].append(obj)
    return found


def read_kubectl(context):
    found = {}
    base = ["kubectl"] + (["--context", context] if context else [])
    for key, (resource, _) in KINDS.items():
        cmd = base + ["get", resource, "-o", "json"]
        if key in ("jobs", "quotas"):
            cmd.insert(len(base) + 1, "-A")
        proc = subprocess.run(cmd, capture_output=True, text=True)
        if proc.returncode != 0:
            # tenants/quotas CRDs may be absent on a minimal install; jobs and nodes are required
            if key in ("jobs", "nodes"):
                sys.exit(f"{' '.join(cmd)} failed: {proc.stderr.strip()}")
            found[key] = []
            continue
        found[key] = flatten([json.loads(proc.stdout)])
    return found


def read_profiles(path):
    """Return {model: set(gpuType)} from Janus' calibrated profiles dir (cheap parse, no YAML dependency)."""
    models = {}
    if not path or not os.path.isdir(path):
        return None
    for name in os.listdir(path):
        if not name.endswith(".yaml"):
            continue
        model, gpus, in_profiles = None, set(), False
        with open(os.path.join(path, name), encoding="utf-8") as f:
            for line in f:
                s = line.rstrip("\n")
                if s.startswith("model:"):
                    model = s.split(":", 1)[1].strip().strip("'\"")
                elif s.startswith("profiles:"):
                    in_profiles = True
                elif in_profiles and s.startswith("  ") and not s.startswith("   ") and s.strip().endswith(":"):
                    gpus.add(s.strip()[:-1])
        if model:
            models[model] = gpus
    return models


def convert_node(n):
    spec = n.get("spec", {})
    keep = {k: spec[k] for k in ("nodeName", "gpuType", "gpuCount", "memoryGB", "rdma", "interconnect") if k in spec}
    keep.setdefault("nodeName", n["metadata"]["name"])
    return {"apiVersion": API_VERSION, "kind": "FabricGpuNode", "metadata": {"name": n["metadata"]["name"]}, "spec": keep}


def convert_quota(q):
    spec = q.get("spec", {})
    gq = spec.get("gpuQuota", {})
    team = spec.get("team") or q["metadata"]["name"]
    fq = {"maxGPUs": gq.get("maxGPUs", 0)}
    if gq.get("maxRunningJobs"):
        fq["maxRunningJobs"] = gq["maxRunningJobs"]
    return {
        "apiVersion": API_VERSION,
        "kind": "FabricQuota",
        "metadata": {"name": q["metadata"]["name"], "namespace": q["metadata"].get("namespace", "default")},
        "spec": {"team": team, "namespaces": spec.get("namespaces") or [], "gpuQuota": fq},
    }


def convert_tenant(t):
    quotas = (t.get("spec") or {}).get("quotas") or {}
    gpus = quotas.get("concurrentGPUs")
    if not gpus:
        return None
    name = t["metadata"]["name"]
    return {
        "apiVersion": API_VERSION,
        "kind": "FabricQuota",
        "metadata": {"name": f"tenant-{name}", "namespace": f"tenant-{name}"},
        "spec": {"team": name, "namespaces": [f"tenant-{name}"], "gpuQuota": {"maxGPUs": gpus}},
    }


def convert_job(j, default_model, default_gpu_type):
    """Return (FabricAIJob, None) or (None, reason)."""
    meta, spec = j["metadata"], j.get("spec", {})
    name = meta["name"]
    gpus = int(spec.get("gpus") or 0)
    if gpus <= 0:
        return None, "CPU-only job (gpus: 0)"
    model = spec.get("model") or default_model
    if not model:
        return None, "no spec.model and no --default-model"
    gpu_type = spec.get("gpuType")
    if not gpu_type or gpu_type == "any":
        gpu_type = default_gpu_type
    if not gpu_type:
        return None, "no spec.gpuType and no GPU type to default to"

    out_spec = {"type": spec.get("type", "training"), "model": model, "gpuType": gpu_type,
                "priority": int(spec.get("priority") or 0)}
    if spec.get("network"):
        out_spec["network"] = spec["network"]
    annotations = {}
    dist = spec.get("distributed") or {}
    nodes = int(dist.get("nodes") or 1)
    if spec.get("migProfile"):
        out_spec["mig"] = {"profile": spec["migProfile"], "count": gpus}
    elif dist.get("enabled"):
        out_spec["distributed"] = {"enabled": True, "nodes": nodes, "gpusPerNode": gpus}
        if nodes > 1:
            annotations["zynera.ai/gang-schedule"] = "true"
            annotations["zynera.ai/gang-size"] = str(nodes)
    else:
        out_spec["gpus"] = gpus
    md = {"name": name, "namespace": meta.get("namespace", "default")}
    if annotations:
        md["annotations"] = annotations
    return {"apiVersion": API_VERSION, "kind": "FabricAIJob", "metadata": md, "spec": out_spec}, None


def write_docs(directory, filename, docs):
    os.makedirs(directory, exist_ok=True)
    with open(os.path.join(directory, filename), "w", encoding="utf-8") as f:
        json.dump({"apiVersion": "v1", "kind": "List", "items": docs}, f, indent=2)
        f.write("\n")


def export(found, out_dir, profiles=None, default_model=None, default_gpu_type=None, strict=False):
    nodes = [convert_node(n) for n in found["nodes"]]
    if not nodes:
        raise SystemExit("no GryviaGpuNode objects: nothing to simulate on")
    if not default_gpu_type:
        counts = collections.Counter(n["spec"].get("gpuType") for n in nodes if n["spec"].get("gpuType"))
        default_gpu_type = counts.most_common(1)[0][0] if counts else None

    quotas = [convert_quota(q) for q in found["quotas"]]
    covered = {ns for q in quotas for ns in q["spec"]["namespaces"]}
    for t in found["tenants"]:
        fq = convert_tenant(t)
        if fq and not (set(fq["spec"]["namespaces"]) & covered):
            quotas.append(fq)

    jobs, skipped = [], []
    for j in found["jobs"]:
        job, reason = convert_job(j, default_model, default_gpu_type)
        if job and profiles is not None:
            m, g = job["spec"]["model"], job["spec"]["gpuType"]
            if m not in profiles:
                job, reason = None, f"model {m!r} has no calibrated profile in the Janus profiles dir"
            elif g not in profiles[m]:
                job, reason = None, f"model {m!r} has no calibrated profile for gpuType {g!r}"
        if job:
            jobs.append(job)
        else:
            skipped.append((f"{j['metadata'].get('namespace', 'default')}/{j['metadata']['name']}", reason))

    if strict and skipped:
        raise SystemExit("jobs that cannot be simulated (--strict):\n" + "\n".join(f"  {n}: {r}" for n, r in skipped))
    if not jobs:
        raise SystemExit("no job could be exported; skipped:\n" + "\n".join(f"  {n}: {r}" for n, r in skipped))

    write_docs(os.path.join(out_dir, "jobs"), "gryvia-jobs.yaml", jobs)
    write_docs(os.path.join(out_dir, "cluster"), "gryvia-gpu-nodes.yaml", nodes)
    if quotas:
        write_docs(os.path.join(out_dir, "quotas"), "gryvia-quotas.yaml", quotas)
    else:
        os.makedirs(os.path.join(out_dir, "quotas"), exist_ok=True)
    total = sum(int(n["spec"].get("gpuCount") or 0) for n in nodes)
    oversized = []
    for jb in jobs:
        s = jb["spec"]
        if "mig" in s:
            continue  # slices of a GPU: the whole-GPU total says nothing
        need = int(s["distributed"]["nodes"]) * int(s["distributed"]["gpusPerNode"]) if "distributed" in s else int(s.get("gpus", 0))
        if need > total:
            oversized.append((f"{jb['metadata']['namespace']}/{jb['metadata']['name']}", need, total))
    return {"jobs": len(jobs), "nodes": len(nodes), "quotas": len(quotas), "skipped": skipped,
            "defaultGpuType": default_gpu_type, "totalGpus": total, "oversized": oversized}


def main(argv=None):
    ap = argparse.ArgumentParser(description=(__doc__ or "").split("\n\n")[0])
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--from-kubectl", action="store_true", help="read the cluster of the current kubectl context")
    src.add_argument("--from-dir", help="directory with JSON/YAML exports of the Gryvia kinds")
    ap.add_argument("--context", help="kubectl context (with --from-kubectl)")
    ap.add_argument("--out", required=True, help="bundle directory to write (jobs/, cluster/, quotas/)")
    ap.add_argument("--profiles-dir", help="Janus calibrated profiles dir; jobs without a profile are skipped")
    ap.add_argument("--default-model", help="model for jobs that set no spec.model")
    ap.add_argument("--default-gpu-type", help="GPU type for jobs with no/any gpuType (default: the most common node type)")
    ap.add_argument("--strict", action="store_true", help="fail instead of skipping jobs that cannot be simulated")
    a = ap.parse_args(argv)

    found = read_kubectl(a.context) if a.from_kubectl else read_dir(a.from_dir)
    profiles = read_profiles(a.profiles_dir) if a.profiles_dir else None
    if a.profiles_dir and profiles is None:
        sys.exit(f"--profiles-dir {a.profiles_dir} is not a directory")
    r = export(found, a.out, profiles, a.default_model, a.default_gpu_type, a.strict)
    print(f"exported {r['jobs']} jobs, {r['nodes']} nodes, {r['quotas']} quotas to {a.out}"
          f" (default gpuType {r['defaultGpuType']})", file=sys.stderr)
    for name, reason in r["skipped"]:
        print(f"  skipped {name}: {reason}", file=sys.stderr)
    for name, need, total in r["oversized"]:
        print(f"  warning {name}: needs {need} GPUs but the cluster has {total}; it will never complete in the simulation",
              file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
