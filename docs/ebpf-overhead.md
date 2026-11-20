# eBPF overhead: what the collector costs

This page reports what the Gryvia eBPF collector costs on one Linux host, how it was measured, how noisy the
measurement is, and what it does **not** tell you. The numbers are real (one run of `scripts/bench-ebpf-overhead.sh`
on 2026-09-30) but they come from a single shared x86 server with loopback traffic only. Treat them as a measured
order of magnitude for a CPU host, not as a promise for your fleet. Re-run the script on your own nodes.

## Summary

- **Syscall-heavy code pays the most.** Five programs hook `raw_tracepoint/sys_enter`, so every syscall on the node
  (all processes, not only pods) runs five small BPF programs. A `getpid()` loop went from 145 ns to 496 ns per call
  (default set) and 524 ns (full set); `read`/`write` of one byte from 250-280 ns to 640-790 ns. That is 2.5x-3.4x on
  a syscall that does no work, and it is far outside the noise (below).
- **Loopback request/response latency** rose by about 7.5 us at p50 (28.6 to 36.1 us) and 11 us at p99 with the
  default set, about 11 us and 22 us with the full set. This is a 64-byte ping-pong, the worst case for per-packet and
  per-syscall probes.
- **Bulk TCP throughput** with the default set was **not distinguishable from noise** (-3.5% single stream, -7% for 16
  streams, while the run-to-run spread of the baseline alone was 30% and 51%). With the full set the median dropped
  about 14-16%, which is probably real but still overlaps the baseline's range.
- **Connect/accept rate** did not show a cost: the medians moved by +11% and +18% (faster, with probes), which is noise,
  not a speed-up. The effect is smaller than this host's noise for that test.
- **The collector process itself** used about 2% of one core idle and about 41% of one core while the load generator
  pushed millions of events per second through it.
- **Kernel-side**, the collector's programs consumed the equivalent of 19-58% of one core (default) and 23-71% (full)
  of run time during the load phases, at 39-114 ns per program run on average. These figures were taken with
  `kernel.bpf_stats_enabled=1`, which itself adds two clock reads per run, so they overstate the cost.

None of this says anything about GPU training or inference throughput. See [Caveats](#caveats).

## What was measured

Everything runs on the host through `scripts/bench-ebpf-overhead.sh`, which starts the **real Go collector binary**
(built with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build`) against a directory of the compiled `ebpf/*.o` objects and
drives a small C load generator (`scripts/bench/loadgen.c`, about 250 lines, no dependencies) over `127.0.0.1`:

| Test | What it does | Reported as |
|---|---|---|
| bulk | one TCP connection, 64 KiB writes | MB/s |
| conns | 16 concurrent TCP streams (16 sender + 16 receiver threads) | aggregate MB/s |
| connrate | 4 client threads doing connect/close against 4 accept threads | connections/s |
| rr | one connection, 64-byte request then 64-byte response, latency of each round trip | p50 / p99 / p99.9 in us |
| syscall | 3 single-threaded loops of `getpid`, `read(/dev/zero, 1)`, `write(/dev/null, 1)` | ns per call |

Each test runs for 3 seconds. One "run" is one collector start followed by the five tests back to back (plus a 3 s
idle window to measure the collector's idle CPU). **Four states**, each run 5 times, interleaved inside every
repetition (baseline, baseline2, default, full, then the next repetition) so slow drift of the shared host affects all
states alike:

| State | Programs |
|---|---|
| `baseline` | no collector |
| `baseline2` | no collector again: the **noise floor** (same conditions, nothing attached) |
| `default` | `tcp_trace`, `latency_probe`, `syscall_monitor`, `dns_tracker` and the security programs `container_escape`, `crypto_detect`, `exfil_detect`, `privesc_monitor`, `driver_fim`, `fingerprint`, `weight_exfil`. 22 programs attached; `dns_tracker` is XDP, so with no `-iface` it loads but is not attached |
| `full` | every `ebpf/*.o` (35 objects). 40 programs attached of 137 in the objects; the rest are uprobes (no NCCL/CUDA libraries are given), XDP/TCX (no `-iface`) and sockops (no `-cgroup-path`) |

Safety rules of the script: only kprobes, tracepoints and raw tracepoints are attached; the collector runs with no
`-iface`, no `-cgroup-path`, no uprobe libraries and no cluster access (`-insecure-listener` on `127.0.0.1:19199`, no
NODE_NAME, no publishing flags); it runs under `timeout` and is stopped with SIGTERM after each run; afterwards the script
checks that no collector process and no new BPF program of the collector's kinds remain and restores
`kernel.bpf_stats_enabled`. Because kprobes and tracepoints are global, the collector inevitably also saw the
node's other workloads (k3s pods) during the window; that is inherent to those hooks, nothing was attached to
their cgroups.

**Two phases.** The first (`perf`) runs with `kernel.bpf_stats_enabled=0` and produces the throughput, latency, syscall
and collector-CPU tables. The second (`kstats`) repeats the same runs with `kernel.bpf_stats_enabled=1` (restored to
0 afterwards) and reads `run_cnt` and `run_time_ns` from `bpftool prog show -j` between the load phases, for the programs
that appeared while the collector ran and executed at least once. Collector CPU is the delta of `utime+stime` in
`/proc/<pid>/stat` divided by wall time.

## Test host

| | |
|---|---|
| Hardware | Intel Xeon E-2336 @ 2.90 GHz, 12 logical CPUs, 31 GB RAM. `systemd-detect-virt` reports `none`: a physical server, not a VM |
| Kernel | Linux 7.0.0-34-generic, x86_64, BTF |
| Shared with | a running k3s cluster and other tenants: load average was 10-12 on 12 CPUs before and during the runs |
| Network | loopback only (`lo`, MTU 65536): no NIC, no offloads, no RDMA |
| GPU | none |
| Tooling | clang 21, bpftool 7.7.0, gcc for the load generator (`-O2 -pthread`), collector built from this repository |

## Results

Median and (min-max) of 5 runs per state. "delta X" is the change of that state's median relative to the `baseline`
median; for latency and ns/call a positive delta is slower, for MB/s and conn/s a negative delta is slower.

### Throughput, latency and syscall rate

| metric | unit | baseline | baseline2 | default | full | delta baseline2 | delta default | delta full |
|---|---|---|---|---|---|---|---|---|
| bulk stream throughput (1 conn) | MB/s | 2,117 (1,539-2,164) | 2,084 (2,070-2,149) | 2,044 (1,978-2,132) | 1,826 (1,316-1,908) | -1.5% | -3.5% | -13.8% |
| 16 parallel streams throughput | MB/s | 11,568 (6,268-12,136) | 11,921 (11,892-12,046) | 10,763 (10,004-11,206) | 9,774 (4,058-10,258) | +3.0% | -7.0% | -15.5% |
| connect+accept+close rate (4+4 threads) | conn/s | 15,333 (15,146-17,426) | 15,773 (15,231-15,973) | 16,990 (16,839-19,427) | 18,076 (11,702-19,184) | +2.9% | +10.8% | +17.9% |
| request/response p50 | us | 28.60 (26.64-34.04) | 28.85 (25.84-31.28) | 36.11 (32.61-36.76) | 39.43 (35.04-52.10) | +0.9% | +26.3% | +37.9% |
| request/response p99 | us | 68.88 (66.92-73.14) | 68.50 (67.62-71.39) | 79.83 (79.00-81.97) | 90.56 (87.32-95.13) | -0.6% | +15.9% | +31.5% |
| request/response p99.9 | us | 108.7 (106.5-157.8) | 119.1 (111.0-237.9) | 136.0 (131.2-157.0) | 150.3 (136.2-197.4) | +9.5% | +25.1% | +38.2% |
| getpid() | ns/call | 145.1 (142.9-161.9) | 148.0 (140.0-161.3) | 495.7 (468.4-543.3) | 523.6 (488.9-649.7) | +2.0% | +241.6% | +260.9% |
| read(/dev/zero, 1) | ns/call | 278.7 (275.8-289.9) | 279.0 (267.7-284.7) | 789.0 (774.8-858.7) | 804.5 (740.2-993.5) | +0.1% | +183.1% | +188.7% |
| write(/dev/null, 1) | ns/call | 249.7 (247.2-255.3) | 250.7 (247.9-254.8) | 635.7 (611.2-705.9) | 691.4 (628.7-910.6) | +0.4% | +154.6% | +176.9% |

### Noise floor (baseline vs baseline, same conditions, no programs attached)

| metric | baseline2 vs baseline (median) | widest min-max spread inside one state |
|---|---|---|
| bulk stream throughput (1 conn) | -1.5% | 30% of median |
| 16 parallel streams throughput | +3.0% | 51% of median |
| connect+accept+close rate (4+4 threads) | +2.9% | 15% of median |
| request/response p50 | +0.9% | 26% of median |
| request/response p99 | -0.6% | 9% of median |
| request/response p99.9 | +9.5% | 107% of median |
| getpid() | +2.0% | 14% of median |
| read(/dev/zero, 1) | +0.1% | 6% of median |
| write(/dev/null, 1) | +0.4% | 3% of median |

### Collector CPU (userspace process, percent of one core; median, min-max)

| state | idle (no load) | during load | attached programs |
|---|---|---|---|
| default | 2.00 (1.33-2.67) | 41.42 (40.53-42.19) | 22 |
| full | 2.33 (2.00-2.33) | 40.96 (13.10-41.44) | 40 |

### Kernel-side cost of the collector's programs (kernel.bpf_stats_enabled=1, per load phase)

Programs counted: kprobe/tracepoint/raw_tracepoint/tracing programs that are new while the collector runs and that ran at least once in the phase. `core-equiv` = sum(run_time_ns) / phase wall time, in percent of one core. Median and min-max over runs.

| state | phase | active programs | run_cnt (median) | run_time ms (median) | avg ns/run | core-equiv % |
|---|---|---|---|---|---|---|
| default | bulk | 21 | 9,736,774 | 562 | 58 | 18.70 (17.90-19.43) |
| default | conns | 22 | 12,677,719 | 1,346 | 106 | 44.57 (41.21-45.67) |
| default | connrate | 21 | 22,362,175 | 1,760 | 79 | 58.46 (57.20-62.38) |
| default | rr | 21 | 12,647,216 | 758 | 60 | 25.15 (23.91-25.32) |
| default | syscall | 21 | 33,411,614 | 1,291 | 39 | 42.73 (41.42-43.31) |
| full | bulk | 39 | 10,943,268 | 698 | 64 | 23.20 (22.48-23.59) |
| full | conns | 40 | 18,325,557 | 2,085 | 114 | 69.05 (67.39-71.80) |
| full | connrate | 40 | 23,786,235 | 2,134 | 90 | 70.85 (67.57-71.18) |
| full | rr | 39 | 13,170,244 | 849 | 64 | 28.16 (27.15-28.92) |
| full | syscall | 39 | 33,857,666 | 1,326 | 39 | 43.89 (42.39-44.58) |

### How to read the noise floor

`baseline2` is the same thing as `baseline`, so the difference between them is pure measurement noise: a few percent
in the medians (up to 9.5% for p99.9 latency). The spread **inside** one state is much larger on this shared host:
15-50% of the median for the throughput and connection-rate tests, 107% for p99.9 latency (a neighbour's burst hits one run), 3-14% for the syscall loops. Rules of thumb used above:

- Anything under roughly 10% in a throughput test is **not resolved** by this experiment.
- The syscall loops are stable (3-14% spread) and the collector's +150-260% is unambiguous.
- Latency medians moved 15-38%, more than the baseline-to-baseline difference (1-10%), but the ranges of baseline and
  the full set touch at p50; report them as "higher by roughly 8-20 us", not as a precise figure.

## Which programs cost what

In the `kstats` phase the same five raw tracepoints on `sys_enter` account for most of the run time in every load phase
(per run, `fingerprint_syscall` about 83-183 ns, `privesc_syscall_monitor` 37-79 ns, `escape_syscall_monitor` 34-66 ns,
`fim_module_monitor` 18-30 ns, `syscall_monitor` 17-29 ns). The TCP kprobes are heavier per call (`tcp_conn_close`
about 1.5 us, `tcp_connect`/`tcp_accept` a similar order, `probe_tcp_sendmsg`/`recvmsg` 270-390 ns) but run far less
often. The full set adds `trace_netif_receive_skb`, `tcp_tuning_probe` and `trace_napi_poll` in the per-packet path
(about 200-240 ns per run, hundreds of thousands to a million runs in the 16-stream phase), which is where its extra
throughput cost comes from. `bpf_stats` adds roughly two `ktime` reads to every run, so these per-run figures are upper
bounds.

## Caveats

Please read these before quoting any number.

- **Bare-metal x86 server, one kernel (7.0.0), one CPU model.** Nothing here was measured on arm64, on a VM, on
  another kernel version, or on a distribution kernel with different tracing configuration. `raw_tracepoint/sys_enter`
  cost depends on the CPU's mitigations and the kernel.
- **Loopback only.** No NIC, no driver, no offloads (GRO/TSO/checksum offload), no XDP/TCX attached (the
  packet-path programs `packet_filter`, `dns_tracker`, `cost_tracker`, `trace_correlator`, `roce_cnp`, `pfc_pause`
  were loaded or skipped but **not** attached to an interface). Real NIC traffic takes a different path and its
  per-packet cost with those programs is **not measured**.
- **No GPU, no RDMA, no NCCL/CUDA uprobes.** The uprobe programs were not attached. Their cost, and the cost of the
  collector next to a real training job, is **not measured**. The syscall-rate microbenchmark is a worst case: a
  training process that issues thousands of syscalls per second, not millions, pays proportionally less.
- **Shared host with noise.** k3s and other tenants were running (load average 10-12 of 12). Repetitions are only 5
  per state; the min-max spread is reported, not a confidence interval.
- **Synthetic load.** 64-byte ping-pong, 64 KiB bulk writes, a tight `connect`/`close` loop and one-byte syscalls are
  micro-benchmarks. They show where cost concentrates (per-syscall and per-packet hooks), not what an application
  would see end to end.
- **The collector's userspace cost depends on event volume.** 41% of one core was measured while the load generator
  triggered roughly 3-11 million BPF program runs per second (from `run_cnt` above); on a quiet node it was 2%.
- **The `default` set is the one defined in the script** (network probes, syscall monitor and the security kprobes);
  it is not read from the chart, so a deployment that enables other programs costs what those cost. `quota_pace` (mutating) and the opt-in
  programs were not attached.
- Single run of the whole campaign. No claim is made that a second campaign would give the same medians.

## Reproduce

```bash
(cd ebpf && make -k)                                                    # on the Linux host (clang, libbpf headers)
(cd collector && CGO_ENABLED=0 go build -o gryvia-collector .)          # or GOOS=linux GOARCH=amd64 to cross-build
sudo scripts/bench-ebpf-overhead.sh --collector collector/gryvia-collector --ebpf-dir ebpf \
  --reps 5 --duration 3 --out /tmp/gryvia-bench-out
scripts/bench-ebpf-overhead.sh --collector x --ebpf-dir y --dry-run     # print the plan, needs nothing
```

The output directory holds `summary.md` (the tables above), `environment.txt`, and `raw/` with every run's load
generator output, CPU counters, `bpftool` snapshots and collector log. The script must run as root on a
machine where attaching kprobes is acceptable; do not point it at a production node during peak load.
