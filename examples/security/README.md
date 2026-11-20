# Security Examples

Example YAML manifests for Gryvia security CRDs providing eBPF-based runtime protection.

> **Status.** The `GryviaSecurityPolicy` controller is registered and checks the eBPF collector and can create block policies, but detection depends on the collector programs (see `ebpf/`), which need a suitable Linux kernel and have not been validated on real workloads.

## Examples

- **[security-policy.yaml](security-policy.yaml)** - `GryviaSecurityPolicy` with container escape, crypto mining, exfiltration, privilege escalation, and driver integrity detection enabled with auto-block

## Usage

```bash
kubectl apply -f security-policy.yaml
```
