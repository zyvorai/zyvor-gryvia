#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Measure what the Gryvia eBPF collector costs on a Linux host: loopback TCP throughput,
# connect/accept rate, request/response latency and syscall rate with no programs attached
# (baseline, run twice to measure the noise floor), the default program set, and the full set.
# Method and results: docs/ebpf-overhead.md.
#
# Run as root on the Linux host (needs bpf(2)); build the pieces first:
#   (cd ebpf && make -k)                                        # the .o files
#   (cd collector && CGO_ENABLED=0 go build -o gryvia-collector .)   # or GOOS=linux GOARCH=amd64 to cross-build
#   sudo scripts/bench-ebpf-overhead.sh --collector collector/gryvia-collector --ebpf-dir ebpf
#
# Safety: only kprobes, tracepoints and raw tracepoints are attached. The collector runs with no
# -iface, no -cgroup-path and no uprobe libraries, so it never touches an interface, a cgroup or
# a shared library. It runs under timeout, and the script checks that it is gone afterwards.
set -euo pipefail

usage() {
	cat <<'USAGE'
usage: bench-ebpf-overhead.sh --collector BIN --ebpf-dir DIR [options]

  --collector BIN     linux collector binary (required)
  --ebpf-dir DIR      directory with the compiled *.o files (required)
  --loadgen BIN       load generator (default: built from scripts/bench/loadgen.c with cc)
  --reps N            repetitions per state (default 5)
  --duration S        seconds per load phase (default 3)
  --out DIR           result directory (default ./bench-out.<timestamp>)
  --phase P           perf (kernel bpf stats off), kstats (on, for run_time_ns/run_cnt) or both (default)
  --states LIST       comma list of baseline,baseline2,default,full (default: all four)
  --dry-run           print the plan and exit
  -h, --help
USAGE
}

collector="" ebpf_dir="" loadgen="" reps=5 dur=3 out="" phase=both dry=0
states="baseline,baseline2,default,full"
while [ $# -gt 0 ]; do
	case "$1" in
	--collector) collector="${2:?}"; shift 2 ;;
	--ebpf-dir) ebpf_dir="${2:?}"; shift 2 ;;
	--loadgen) loadgen="${2:?}"; shift 2 ;;
	--reps) reps="${2:?}"; shift 2 ;;
	--duration) dur="${2:?}"; shift 2 ;;
	--out) out="${2:?}"; shift 2 ;;
	--phase) phase="${2:?}"; shift 2 ;;
	--states) states="${2:?}"; shift 2 ;;
	--dry-run) dry=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 64 ;;
	esac
done
[ -n "$collector" ] && [ -n "$ebpf_dir" ] || { usage >&2; exit 64; }
case "$phase" in perf | kstats | both) ;; *) echo "bad --phase: $phase" >&2; exit 64 ;; esac
case "$reps" in '' | *[!0-9]*) echo "bad --reps: $reps" >&2; exit 64 ;; esac

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -n "$out" ] || out="./bench-out.$(date +%Y%m%d-%H%M%S)"

# The default set: what the chart's collector runs on a node without RDMA, GPU libraries or an
# XDP/TCX interface configured (dns_tracker is XDP, so without -iface it loads but does not attach).
default_objs="tcp_trace latency_probe syscall_monitor dns_tracker container_escape crypto_detect exfil_detect privesc_monitor driver_fim fingerprint weight_exfil"

if [ "$dry" = 1 ]; then
	echo "collector=$collector ebpf_dir=$ebpf_dir reps=$reps duration=${dur}s phase=$phase states=$states out=$out"
	echo "default set: $default_objs"
	echo "full set: every *.o in $ebpf_dir (no -iface, -cgroup-path or uprobe libraries: XDP/TCX/sockops/uprobe programs stay unattached)"
	echo "per run: idle ${dur}s, then loadgen bulk, conns(16), connrate(4), rr, syscall (each ${dur}s)"
	exit 0
fi

[ "$(id -u)" = 0 ] || { echo "run as root (bpf(2) needs it)" >&2; exit 77; }
[ -x "$collector" ] || { echo "collector not executable: $collector" >&2; exit 66; }
[ -d "$ebpf_dir" ] || { echo "no such directory: $ebpf_dir" >&2; exit 66; }
command -v bpftool >/dev/null || { echo "bpftool not found" >&2; exit 69; }
command -v python3 >/dev/null || { echo "python3 not found" >&2; exit 69; }

work="$(mktemp -d /tmp/gryvia-bench.XXXXXX)"
mkdir -p "$out/raw"
orig_stats="$(sysctl -n kernel.bpf_stats_enabled)"
tpid=""
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() {
	set +e
	[ -n "$tpid" ] && kill -TERM "$tpid" 2>/dev/null
	sysctl -qw kernel.bpf_stats_enabled="$orig_stats"
	rm -rf "$work"
}
trap cleanup EXIT

if [ -z "$loadgen" ]; then
	command -v cc >/dev/null || { echo "cc not found and no --loadgen given" >&2; exit 69; }
	loadgen="$work/loadgen"
	cc -O2 -pthread -o "$loadgen" "$here/bench/loadgen.c"
fi

# Object sets.
mkdir -p "$work/objs-default" "$work/objs-full"
for o in $default_objs; do
	[ -f "$ebpf_dir/$o.o" ] || { echo "missing $ebpf_dir/$o.o (run make in ebpf/)" >&2; exit 66; }
	cp "$ebpf_dir/$o.o" "$work/objs-default/"
done
cp "$ebpf_dir"/*.o "$work/objs-full/"

ids_before="$(bpftool prog show -j | python3 -c 'import json,sys; print(" ".join(str(p["id"]) for p in json.load(sys.stdin)))')"

snap_progs() { bpftool prog show -j >"$1"; }

# One measured run: run_one PHASE STATE REP
run_one() {
	local ph="$1" st="$2" rep="$3" base cpid="" sdir="" log t0 t1 ticks0 ticks1 idle0 idle1
	base="$out/raw/$ph.$st.$rep"
	case "$st" in default) sdir=objs-default ;; full) sdir=objs-full ;; esac
	if [ -n "$sdir" ]; then
		log="$base.collector.log"
		# The timeout is a backstop: 60 s of load plus start-up.
		timeout -s TERM $((dur * 8 + 60)) "$collector" -ebpf-dir "$work/$sdir" \
			-metrics-addr 127.0.0.1:19199 -insecure-listener >"$log" 2>&1 &
		tpid=$!
		for _ in $(seq 1 60); do
			grep -q "eBPF load complete" "$log" 2>/dev/null && break
			sleep 0.5
		done
		grep -q "eBPF load complete" "$log" || { echo "collector did not attach (see $log)" >&2; exit 1; }
		cpid="$(pgrep -P "$tpid" | head -n1)"
		[ -n "$cpid" ] || { echo "collector process not found" >&2; exit 1; }
		sleep 2
		grep -o '"attached":[0-9]*' "$log" | tail -n1 >"$base.attached"
	fi
	local hz
	hz="$(getconf CLK_TCK)"
	echo "$hz" >"$base.clk_tck"
	# Collector CPU: idle window, then the load window.
	if [ -n "$cpid" ]; then
		idle0="$(awk '{print $14+$15}' "/proc/$cpid/stat")"
		[ "$ph" = kstats ] && snap_progs "$base.prog0.json"
	fi
	sleep "$dur"
	if [ -n "$cpid" ]; then
		idle1="$(awk '{print $14+$15}' "/proc/$cpid/stat")"
		ticks0="$idle1"
		[ "$ph" = kstats ] && snap_progs "$base.prog1.json"
	fi
	t0="$(date +%s.%N)"
	: >"$base.loadgen"
	local mode args
	for mode in bulk conns connrate rr syscall; do
		args=()
		[ "$mode" = conns ] && args=(-n 16)
		[ "$mode" = connrate ] && args=(-n 4)
		s0="$(date +%s.%N)"
		"$loadgen" "$mode" -d "$dur" ${args[@]+"${args[@]}"} >>"$base.loadgen"
		s1="$(date +%s.%N)"
		# Per-phase kernel-side counters, so the cost can be tied to the workload that caused it.
		if [ -n "$cpid" ] && [ "$ph" = kstats ]; then
			snap_progs "$base.prog.$mode.json"
			echo "$s0 $s1" >"$base.prog.$mode.wall"
		fi
	done
	t1="$(date +%s.%N)"
	if [ -n "$cpid" ]; then
		ticks1="$(awk '{print $14+$15}' "/proc/$cpid/stat")"
		echo "$idle0 $idle1 $ticks0 $ticks1 $t0 $t1 $dur" >"$base.cpu"
		kill -TERM "$tpid" 2>/dev/null || true
		wait "$tpid" 2>/dev/null || true
		tpid=""
		if kill -0 "$cpid" 2>/dev/null; then echo "collector $cpid still running" >&2; exit 1; fi
	fi
	echo "  $ph $st rep $rep done"
}

phases=()
case "$phase" in perf) phases=(perf) ;; kstats) phases=(kstats) ;; both) phases=(perf kstats) ;; esac
IFS=',' read -r -a state_list <<<"$states"

# Programs that already exist on the host (k3s, systemd) are recorded so the kernel-side numbers can
# be attributed to the collector's programs only (the ones that are new while it runs).
echo "$ids_before" >"$out/prog_ids_before.txt"
{
	echo "date: $(date -u +%FT%TZ)"
	echo "kernel: $(uname -r)"
	echo "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //') x $(nproc)"
	echo "load average before: $(cut -d' ' -f1-3 /proc/loadavg)"
	echo "bpf_stats_enabled before: $orig_stats"
	echo "collector: $collector"
	echo "reps=$reps duration=${dur}s states=$states"
} >"$out/environment.txt"

for ph in "${phases[@]}"; do
	if [ "$ph" = kstats ]; then sysctl -qw kernel.bpf_stats_enabled=1; else sysctl -qw kernel.bpf_stats_enabled=0; fi
	echo "== phase $ph (bpf_stats_enabled=$(sysctl -n kernel.bpf_stats_enabled))"
	for rep in $(seq 1 "$reps"); do
		# Interleave the states inside every repetition so slow drift of the shared host hits all of them.
		for st in "${state_list[@]}"; do run_one "$ph" "$st" "$rep"; done
	done
done
sysctl -qw kernel.bpf_stats_enabled="$orig_stats"

# Leftover checks.
left=0
if pgrep -x "$(basename "$collector" | cut -c1-15)" >/dev/null 2>&1; then echo "LEFTOVER: collector process" >&2; left=1; fi
new_ids="$(bpftool prog show -j | python3 -c '
import json, sys
before = set(map(int, open(sys.argv[1]).read().split()))
kinds = {"kprobe", "tracepoint", "raw_tracepoint", "tracing", "perf_event"}
print(" ".join(str(p["id"]) for p in json.load(sys.stdin) if p["id"] not in before and p["type"] in kinds))
' "$out/prog_ids_before.txt")"
if [ -n "$new_ids" ]; then echo "LEFTOVER: bpf programs: $new_ids" >&2; left=1; fi
echo "bpf_stats_enabled restored to $(sysctl -n kernel.bpf_stats_enabled)" | tee -a "$out/environment.txt"
python3 "$here/bench/summarize.py" "$out" >"$out/summary.md"
cat "$out/summary.md"
[ "$left" = 0 ] && echo "cleanup check: no collector process, no leftover bpf programs" | tee -a "$out/environment.txt"
exit "$left"
