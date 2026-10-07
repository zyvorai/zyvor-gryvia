# GPU reset agent

**Status: unit-tested with fake clients, a fake command runner and a fake rebooter; dry-run is the default and the only mode CI exercises.** `nvidia-smi --gpu-reset`, the driver reload and the reboot have never been run by this agent on a real GPU node, in a container, or against a GPU Operator driver container. Treat every execute path as unverified (not run on hardware).

## What it is

An optional DaemonSet (`gpuOperator.resetAgent.enabled`, off by default) on nodes labelled `nvidia.com/gpu.present=true`. One agent acts on its own node only (`NODE_NAME`).

You ask for a reset by annotating the node:

```bash
kubectl cordon gpu-node-1
kubectl drain gpu-node-1 --ignore-daemonsets --delete-emptydir-data   # or let the health check quarantine/drain it
kubectl annotate node gpu-node-1 gryvia.io/gpu-reset-request=reset-2026-10-02-a gryvia.io/gpu-reset-gpus=0,1
kubectl get node gpu-node-1 -o jsonpath='{.metadata.annotations.gryvia\.io/gpu-reset-result}'
```

`gryvia.io/gpu-reset-gpus` is optional (empty means all GPUs). `gryvia.io/gpu-reset-action` picks the action: `reset` (default), `driver-reload` or `reboot` (see [Recovery actions](#recovery-actions)). The agent acts only when the node is cordoned **and** no non-terminated pod other than DaemonSet/static pods requests `nvidia.com/gpu`; otherwise it records `Blocked` with the reason and re-checks every poll. A new request id is a new request; an id that was handled (`Done`, `Failed`, `DryRun`, `Invalid`) is never retried automatically, so a failed reset is not hammered. Results are JSON in `gryvia.io/gpu-reset-result` (`id`, `state`, `message`, `at`, plus `action`, `started`, `bootID`, `apps` while a driver reload or reboot is `InProgress`) and the handled id is in `gryvia.io/gpu-reset-observed`.

Safety properties, all covered by unit tests: dry-run by default and in dry-run nothing is executed; GPU indices are parsed as integers 0-63 (no shell, no free-form arguments, the only command is `nvidia-smi --gpu-reset [-i list]`); the request id is restricted to 63 characters of `[A-Za-z0-9._-]`; the agent never cordons, drains, uncordons or removes taints; it never touches another node. Unknown actions are `Invalid`.

## Turning on real resets

```yaml
gpuOperator:
  resetAgent:
    enabled: true
    execute: true
    nvidiaSmi: /run/nvidia/driver/usr/bin/nvidia-smi   # a path the container can run
    driverRoot: /run/nvidia/driver                     # hostPath providing it, mounted read-only
```

The container is then `privileged`, because a GPU reset needs device access. The agent image is distroless and contains no `nvidia-smi`; whether the host's binary runs from a mounted driver root (it needs its libraries too) depends on how the driver is installed, so test on one node first. Helm refuses `execute` without `nvidiaSmi`.

## Recovery actions

Two heavier actions are off unless the chart allows them; without permission the request is `Invalid`.

| Action | Allowed by | What the agent does (with `execute`) | `Done` when |
|---|---|---|---|
| `driver-reload` | `resetAgent.allowDriverReload` | Deletes this node's GPU Operator pods labelled `app=nvidia-driver-daemonset` and `app=nvidia-device-plugin-daemonset`, so their DaemonSets recreate them and the driver is reloaded | Every deleted app has a Ready pod on the node created after the start (failed after 15 min) |
| `reboot` | `resetAgent.allowReboot` | `rebootMethod: kured` (default) writes `<rebootSentinelDir>/reboot-required` for [kured](https://kured.dev) to drain and reboot; `nsenter` runs `systemctl reboot` in the host PID namespace (`hostPID`, privileged) | The node's boot ID changed and it is Ready (failed after 30 min) |

Both take a `coordination.k8s.io` Lease (`gryvia-gpu-recovery` in the release namespace) so only one node reloads or reboots at a time; a node waiting for it reports `Blocked` with the holder's name. The lease is renewed while the action runs and released when it finishes or fails. The same cordon and no-GPU-pods checks as a reset apply first. In dry-run the agent deletes nothing, writes no sentinel and takes no lease.

```yaml
gpuOperator:
  resetAgent:
    enabled: true
    execute: true
    nvidiaSmi: /run/nvidia/driver/usr/bin/nvidia-smi
    driverRoot: /run/nvidia/driver
    allowDriverReload: true      # adds pods delete to the agent's ClusterRole
    allowReboot: true
    rebootMethod: kured          # kured must run with --reboot-sentinel=/var/run/gryvia/reboot-required
    rebootSentinelDir: /var/run/gryvia
```

The chart adds the lease Role, pod delete only with `allowDriverReload`, the sentinel hostPath (`DirectoryOrCreate`) only for kured, and `hostPID` only for nsenter; `scripts/tests/gpu-recovery-chart.test.sh` checks these renders.

## Escalation ladder (health check)

With `platformCompletion.gpuRemediation`, a `GryviaHealthCheck` whose `onFailure.autoRemediate` is true and whose `spec.remediation` enables steps drives quarantined nodes through them in order: `gpuReset`, then `driverReload`, then `nodeReboot`, at most `maxAttempts` requests per quarantine (default: one per enabled step).

```yaml
spec:
  onFailure: {cordon: true, drain: true, autoRemediate: true}
  remediation: {gpuReset: true, driverReload: true, nodeReboot: true, maxAttempts: 3}
```

- It acts only on nodes this health check quarantined (`gryvia.io/quarantine-owner` is its UID). It writes `gryvia.io/gpu-reset-request` (`gryvia-<unix>-<attempt>`), `gryvia.io/gpu-reset-action` and `gryvia.io/gpu-recovery-attempts`, then waits while the agent reports `Blocked` (for example until the drain finishes) or `InProgress`.
- Each agent result is recorded once in `status.remediationHistory`.
- After `Done`, it waits for a health-check run later than the result's `at`. If the node passes, it uncordons, removes the `gryvia.io/gpu-unhealthy` taint and clears the quarantine and recovery annotations (history action `uncordon`). If it still fails, or the action `Failed`, the next step is requested.
- A `DryRun` result stops the ladder (condition reason `AgentDryRun`): the agent is not executing, so escalating would change nothing.
- When attempts reach the bound and the node is still unhealthy, the node stays quarantined and the `RemediationReady` condition is `False` with reason `RemediationExhausted`. To try again, fix the node and remove `gryvia.io/gpu-recovery-attempts` and the `gryvia.io/gpu-reset-*` annotations, or uncordon it by hand.
- While a step is pending the condition reason is `Recovering`; after a lift it is `Recovered`.

A node that becomes healthy before any request was made is left quarantined for a person, as before.

## Limits

- Not run on hardware: the reset, the driver reload against the GPU Operator driver container, kured and nsenter reboots have only been exercised with fakes.
- The driver reload assumes the NVIDIA GPU Operator's default `app` labels; a host-installed driver cannot be reloaded this way (use reboot).
- A reset can fail while something else (a monitoring agent, MIG manager, persistence mode) holds the GPU; the failure text is recorded, not interpreted.
- The nsenter reboot does not drain; rely on the health check's drain (`onFailure.drain`) or use kured, which drains itself.
- Unverified: container privilege model across distros, MIG-enabled GPUs, NVSwitch/Fabric Manager systems (resetting one GPU in an NVSwitch domain may need the whole domain).
