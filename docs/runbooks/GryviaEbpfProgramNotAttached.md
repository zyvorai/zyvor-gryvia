# GryviaEbpfProgramNotAttached

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_ebpf_program_attached == 0` for 5 minutes.

## What it means

An eBPF program in the collector failed to attach on a node, so the signals it feeds are missing there (they read as absent or zero, which can look healthy).

## How to check

```bash
kubectl -n gryvia-network get pods -l app.kubernetes.io/component=collector -o wide
kubectl -n gryvia-network logs <collector-pod> | grep -i -E 'attach|verifier|btf|failed'
gryvia_ebpf_program_attached == 0   # labels object, program, kind say which one
# Same list from the collector:  GET /api/v1/ebpf/status   (signed; see the README)
uname -r; ls /sys/kernel/btf/vmlinux   # on the node
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Kernel without BTF or too old for the program; missing kernel config.
- Missing capabilities (CAP_BPF, CAP_PERFMON) or seccomp/AppArmor blocking bpf().
- Program needs hardware absent on the node (RDMA, NVIDIA userspace libraries for uprobes): expected on such nodes.
- Interface/driver without XDP support (PFC/CNP programs).

Programs the configuration did not ask for are not failures: they have no `gryvia_ebpf_program_attached` series and
do not fire this alert. They show in `GET /api/v1/ebpf/status` as skipped with `notRequested: true`. That covers:

- opt-in programs that were not enabled (`-enable-programs`, chart `ebpf.enablePrograms`);
- opt-in features that are off: quota pacing (`-quota-pace`), the libibverbs probes (`-ibverbs-probes`), `xdp_mux`
  (`-xdp-mux`), and `infer_latency` / `infer_ttft` with an empty `-infer-ports`;
- XDP and TCX programs when no `-iface` is set, and sockops / sk_msg programs when no `-cgroup-path` is set.

A program whose node lacks a library or symbol, whose interface already has another XDP program, or whose attach
failed is not "not requested": it exports 0 and can fire this alert, which is what the causes above describe.

## Mitigation

- Fix the kernel/capability cause shown in the log, then restart the collector pod.
- If the program is not applicable to the node class, disable it via the chart (`ebpf.*` flags) so it stops alerting.

## Limits (honest)

The gauge shows attach state, not whether the program sees traffic. Verified on one x86 Linux 7.0 test host only; other kernels are untested.
