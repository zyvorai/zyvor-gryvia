# GryviaPodCrashLooping

Severity: critical. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

A container in a namespace starting with `gryvia` restarted more than 3 times in 15 minutes (kube-state-metrics).

## What it means

A Gryvia pod (operator, gateway, UI, collector) is repeatedly crashing.

## How to check

```bash
kubectl get pods -A | grep -i gryvia | grep -v Running
kubectl -n <ns> describe pod <pod>   # last state, exit code, OOMKilled
kubectl -n <ns> logs <pod> --previous
kubectl -n <ns> get events --sort-by=.lastTimestamp | tail -20
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- OOMKilled: memory limit too low (collector on busy nodes; see collector-health dashboard).
- Bad configuration or missing Secret referenced by the pod.
- Collector cannot load eBPF programs on this kernel (it exits when required probes fail).
- Image pull or startup dependency failing.

## Mitigation

- Address the cause from `--previous` logs and the describe output; raise the memory limit if OOMKilled.
- Roll back if it began with an upgrade: `helm rollback <release>`.

## Limits (honest)

Requires kube-state-metrics; with none installed this alert never fires.
