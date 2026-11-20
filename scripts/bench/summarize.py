#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Summarise the raw files written by scripts/bench-ebpf-overhead.sh into markdown tables."""
import glob
import json
import os
import statistics
import sys

out = sys.argv[1]
raw = os.path.join(out, "raw")
KINDS = {"kprobe", "tracepoint", "raw_tracepoint", "tracing", "perf_event"}

# metric key -> (label, unit, higher_is_better)
METRICS = [
    ("bulk.mb_per_s", "bulk stream throughput (1 conn)", "MB/s", True),
    ("conns.mb_per_s", "16 parallel streams throughput", "MB/s", True),
    ("connrate.conn_per_s", "connect+accept+close rate (4+4 threads)", "conn/s", True),
    ("rr.p50_us", "request/response p50", "us", False),
    ("rr.p99_us", "request/response p99", "us", False),
    ("rr.p999_us", "request/response p99.9", "us", False),
    ("syscall.getpid_ns", "getpid()", "ns/call", False),
    ("syscall.read_ns", "read(/dev/zero, 1)", "ns/call", False),
    ("syscall.write_ns", "write(/dev/null, 1)", "ns/call", False),
]


def load_runs():
    runs = {}
    for lg in sorted(glob.glob(os.path.join(raw, "*.loadgen"))):
        ph, st, rep, _ = os.path.basename(lg).split(".")
        base = lg[: -len(".loadgen")]
        m = {}
        for line in open(lg):
            line = line.strip()
            if not line.startswith("{"):
                continue
            d = json.loads(line)
            mode = d.pop("mode")
            for k, v in d.items():
                m[f"{mode}.{k}"] = v
        cpu = None
        if os.path.exists(base + ".cpu"):
            i0, i1, l0, l1, t0, t1, dur = map(float, open(base + ".cpu").read().split())
            hz = float(open(base + ".clk_tck").read())
            cpu = {
                "idle": (i1 - i0) / hz / dur * 100,
                "load": (l1 - l0) / hz / (float(t1) - float(t0)) * 100,
            }
        runs.setdefault((ph, st), []).append({"m": m, "cpu": cpu, "base": base})
    return runs


def stats(vals):
    vals = sorted(vals)
    return statistics.median(vals), vals[0], vals[-1]


def fmt(v):
    return f"{v:,.0f}" if abs(v) >= 1000 else (f"{v:.1f}" if abs(v) >= 100 else f"{v:.2f}")


def cell(vals):
    med, lo, hi = stats(vals)
    return f"{fmt(med)} ({fmt(lo)}-{fmt(hi)})"


def delta(a, b):
    return (b - a) / a * 100 if a else float("nan")


def perf_tables(runs, lines):
    order = [s for s in ("baseline", "baseline2", "default", "full") if ("perf", s) in runs]
    if not order:
        return
    n = len(runs[("perf", order[0])])
    lines.append(f"### Throughput, latency and syscall rate (median, min-max of {n} runs; delta = change of the median vs baseline)\n")
    head = "| metric | unit | " + " | ".join(order) + " | " + " | ".join(f"delta {s}" for s in order[1:]) + " |"
    lines.append(head)
    lines.append("|" + "---|" * (2 + len(order) + len(order) - 1))
    noise = []
    for key, label, unit, hib in METRICS:
        col, meds = [], {}
        for s in order:
            vals = [r["m"][key] for r in runs[("perf", s)] if key in r["m"]]
            if not vals:
                col.append("n/a")
                continue
            meds[s] = stats(vals)[0]
            col.append(cell(vals))
        ds = []
        for s in order[1:]:
            ds.append(f"{delta(meds['baseline'], meds[s]):+.1f}%" if s in meds and "baseline" in meds else "n/a")
        lines.append(f"| {label} | {unit} | " + " | ".join(col) + " | " + " | ".join(ds) + " |")
        if "baseline" in meds and "baseline2" in meds:
            spread = max((stats([r["m"][key] for r in runs[("perf", s)]])[2] - stats([r["m"][key] for r in runs[("perf", s)]])[1]) / meds[s] * 100 for s in ("baseline", "baseline2"))
            noise.append((label, delta(meds["baseline"], meds["baseline2"]), spread))
    lines.append("")
    if noise:
        lines.append("### Noise floor (baseline vs baseline, same conditions, no programs attached)\n")
        lines.append("| metric | baseline2 vs baseline (median) | widest min-max spread inside one state |")
        lines.append("|---|---|---|")
        for label, d, sp in noise:
            lines.append(f"| {label} | {d:+.1f}% | {sp:.0f}% of median |")
        lines.append("")
    lines.append("### Collector CPU (userspace process, percent of one core; median, min-max)\n")
    lines.append("| state | idle (no load) | during load | attached programs |")
    lines.append("|---|---|---|---|")
    for s in order:
        if s.startswith("baseline"):
            continue
        rs = runs[("perf", s)]
        att = ""
        p = rs[0]["base"] + ".attached"
        if os.path.exists(p):
            att = open(p).read().strip().split(":")[-1]
        lines.append(f"| {s} | {cell([r['cpu']['idle'] for r in rs])} | {cell([r['cpu']['load'] for r in rs])} | {att} |")
    lines.append("")


def kernel_tables(runs, lines):
    before = set(map(int, open(os.path.join(out, "prog_ids_before.txt")).read().split()))
    order = [s for s in ("default", "full") if ("kstats", s) in runs]
    if not order:
        return
    modes = ["bulk", "conns", "connrate", "rr", "syscall"]
    lines.append("### Kernel-side cost of the collector's programs (kernel.bpf_stats_enabled=1, per load phase)\n")
    lines.append("Programs counted: kprobe/tracepoint/raw_tracepoint/tracing programs that are new while the collector runs and that ran at "
                 "least once in the phase. `core-equiv` = sum(run_time_ns) / phase wall time, in percent of one core. Median and min-max over runs.\n")
    lines.append("| state | phase | active programs | run_cnt (median) | run_time ms (median) | avg ns/run | core-equiv % |")
    lines.append("|---|---|---|---|---|---|---|")
    top = {}
    for s in order:
        for i, mode in enumerate(modes):
            cnts, times, cores, nprog = [], [], [], []
            for r in runs[("kstats", s)]:
                b = r["base"]
                prev = b + ".cpu" if i == 0 else f"{b}.prog.{modes[i - 1]}.json"
                p1 = {p["id"]: p for p in json.load(open(b + ".prog1.json" if i == 0 else prev))}
                p2 = {p["id"]: p for p in json.load(open(f"{b}.prog.{mode}.json"))}
                s0, s1 = map(float, open(f"{b}.prog.{mode}.wall").read().split())
                ours = [k for k, p in p2.items() if k not in before and p["type"] in KINDS and k in p1
                        and p2[k].get("run_cnt", 0) > p1[k].get("run_cnt", 0)]
                c = sum(p2[k]["run_cnt"] - p1[k].get("run_cnt", 0) for k in ours)
                t = sum(p2[k]["run_time_ns"] - p1[k].get("run_time_ns", 0) for k in ours)
                cnts.append(c)
                times.append(t / 1e6)
                cores.append(t / 1e9 / (s1 - s0) * 100)
                nprog.append(len(ours))
                if r is runs[("kstats", s)][len(runs[("kstats", s)]) // 2]:
                    top.setdefault(s, {})[mode] = sorted(
                        ((p2[k]["name"], p2[k]["type"], p2[k]["run_cnt"] - p1[k].get("run_cnt", 0),
                          p2[k]["run_time_ns"] - p1[k].get("run_time_ns", 0)) for k in ours), key=lambda x: -x[3])[:5]
            mc, mt = statistics.median(cnts), statistics.median(times)
            avg = mt * 1e6 / mc if mc else 0
            lines.append(f"| {s} | {mode} | {int(statistics.median(nprog))} | {mc:,.0f} | {mt:,.0f} | {avg:.0f} | {cell(cores)} |")
    lines.append("")
    for s in order:
        lines.append(f"Top programs by run time per phase, {s} (one representative run):\n")
        lines.append("| phase | program | type | run_cnt | run_time ms | avg ns |")
        lines.append("|---|---|---|---|---|---|")
        for mode in modes:
            for name, typ, c, t in top.get(s, {}).get(mode, []):
                lines.append(f"| {mode} | {name} | {typ} | {c:,} | {t / 1e6:,.1f} | {t / c if c else 0:.0f} |")
        lines.append("")
    lines.append("With `kernel.bpf_stats_enabled=1` every program run also pays two clock reads, so this phase's throughput and latency are "
                 "not used in the tables above.\n")


runs = load_runs()
lines = []
perf_tables(runs, lines)
kernel_tables(runs, lines)
print("\n".join(lines))
