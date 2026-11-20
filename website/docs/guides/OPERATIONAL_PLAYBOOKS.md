# Operational Playbooks

Standard operating procedures and runbooks for Gryvia operations.

> Run `gryvia <command> --help` for options. Commands not shown here are not implemented yet; the playbooks use `kubectl` for those steps.
>
> These are generic runbook templates written for this CLI and CRDs, not procedures validated on a production GPU fleet. The CLI reads the Kubernetes API through your kubeconfig. Several diagnostics depend on optional components that are off by default or unverified: `gryvia gpu nccl/training` and `gryvia network ...` read `GryviaTrainingInsight` and network-intelligence objects that only fill in when the network-intelligence operator and the eBPF collector are running (see [Network Intelligence](./NETWORK_INTELLIGENCE.md)), and `gryvia gpu rdma` / `gryvia gpu memory` print hints or placeholders rather than measurements. Team names, node names and thresholds below are examples.

## Table of Contents

1. [Emergency Response](#emergency-response)
2. [Performance Issues](#performance-issues)
3. [Cost Management](#cost-management)
4. [Capacity Planning](#capacity-planning)
5. [Maintenance Procedures](#maintenance-procedures)
6. [Incident Response](#incident-response)

---

## Emergency Response

### GPU Node Failure

**Symptoms:**
- Jobs failing with GPU errors
- Node marked as NotReady
- DCGM reporting hardware failures

**Response Steps:**

```bash
# 1. Identify the failed node
gryvia health gpu
gryvia list nodes

# 2. Cordon node immediately and record why
gryvia maintenance start gpu-node-05 --reason "GPU hardware failure"

# 3. List affected jobs (job pods carry the label gryvia.io/job=<name>)
kubectl get pods -A -o wide | grep gpu-node-05

# 4. Drain node gracefully through the Eviction API (respects PodDisruptionBudgets;
#    blocked pods are reported and left running). Evicted job pods are rescheduled
#    on other nodes if the job allows it; otherwise cancel and resubmit the job.
gryvia maintenance start gpu-node-05 --drain

# 5. Inspect the node and its events
kubectl describe node gpu-node-05
kubectl get events -A --field-selector involvedObject.name=gpu-node-05

# 6. Check GPU-level details reported by Gryvia
gryvia get node gpu-node-05

# 7. Record the incident in your ticketing system and, if hardware
#    replacement is needed, schedule a maintenance window with the data center team
```

**Recovery:**

```bash
# After hardware replacement:
# 1. Uncordon node and clear the maintenance marker
gryvia maintenance end gpu-node-05

# 2. Verify health
gryvia health gpu
gryvia get node gpu-node-05

# 3. Run a small test job on the node (see the job spec reference for the format)
gryvia validate test-job.yaml
gryvia submit -f test-job.yaml --wait

# 4. Close the incident in your ticketing system
```

---

### Cluster at Capacity

**Symptoms:**
- Many jobs stuck in Pending
- Long queue times
- Budget still available

**Response Steps:**

```bash
# 1. Check current capacity, per GPU type, including pending demand and shortfall
gryvia capacity
gryvia cluster --detailed

# 2. View queue
gryvia queue

# 3. Identify the jobs holding GPUs
gryvia list jobs -a

# 4. Options:

# Option A: Cancel or resubmit lower-priority jobs to free GPUs
gryvia cancel <job-name>

# Option B: Add GPU nodes (see Capacity Planning) and check they appear
gryvia list nodes

# 5. Communicate the situation to users through your usual channels
#    (for example the team chat channel)
```

---

### Out of Budget

**Symptoms:**
- Jobs blocked with BudgetExceeded
- Alerts about budget consumption

**Response Steps:**

```bash
# 1. Check budget status
gryvia quota ml-research --budget

# 2. Analyze spending
gryvia cost ml-research --detailed

# 3. Identify cost drivers
gryvia list jobs

# 4. Options:

# Option A: Request a budget increase
# Ask the team lead or finance owner, then update the team's GryviaQuota:
kubectl edit gryviaquota <quota-name>

# Option B: Reduce spending
# Cancel low-priority jobs
gryvia cancel <job-name>

# 5. Set up budget alerts with your monitoring stack (for example Prometheus alert rules)
```

---

## Performance Issues

### Slow Training Job

**Diagnosis:**

```bash
# 1. Check job status and recent logs
gryvia status training-job-42
gryvia logs training-job-42 --tail 200

# 2. Check training insights
gryvia gpu training --job training-job-42

# 3. Check job events
kubectl describe gryviaaijob training-job-42

# 4. Check network
gryvia network status

# 5. Check collective communication
gryvia gpu nccl --job training-job-42
```

**Common fixes (generic PyTorch practice):**

These are training-script settings, not Gryvia features. Gryvia passes `spec.env` and `spec.command` to your container;
whether a variable such as the ones below has any effect depends entirely on your code.

```yaml
spec:
  env:
    - name: NUM_WORKERS      # only if your script reads it
      value: "16"
  command:
    - python
    - train.py
    - --compile=True         # only if your script defines the flag
```

Typical levers are mixed precision, more data loader workers, a larger batch size and `torch.compile`; check GPU
utilisation (`kubectl exec <pod> -- nvidia-smi`, or DCGM metrics in Prometheus) before and after each change.

---

### Poor Multi-Node Scaling

**Diagnosis:**

```bash
# 1. Check NCCL collective operation stats
gryvia gpu nccl --job training-job-42

# 2. Check RDMA and GPU memory transfer stats on the nodes
gryvia gpu rdma --node gpu-node-01
gryvia gpu memory --node gpu-node-01

# 3. Check network health and anomalies
gryvia network status
gryvia network anomalies

# 4. View NCCL logs
gryvia logs training-job-42 --tail 500 | grep NCCL
```

**Common fixes:**

```yaml
# The job's network mode is a string (rdma or sriov); it adds the NetworkAttachmentDefinition
# annotation and, for distributed jobs, NCCL_IB_DISABLE=0 and NCCL_NET_GDR_LEVEL=5
spec:
  network: rdma
  env:
    - name: NCCL_SOCKET_IFNAME
      value: "ib0"          # depends on your fabric
    - name: NCCL_DEBUG
      value: "INFO"
```

`GryviaAIJob` has no topology-aware placement or GPUDirect fields. Node selection is a filter/score over node labels and
free GPUs; use `spec.nodeSelector` or `spec.affinity` to steer placement. RDMA needs the hardware, drivers and Multus
set up by you (see [Storage and Network Operators](./STORAGE_NETWORK_OPERATORS.md)); none of it has been validated on
real RDMA hardware by the project.

---

## Cost Management

### Monthly Cost Review

**Process:**

```bash
# 1. Review spending for the month, all teams
gryvia cost --period month --detailed

# 2. Review a single team
gryvia cost ml-research --period month --detailed

# 3. Compare against quota and budget
gryvia quota --budget

# 4. Identify long-running or large jobs
gryvia list jobs -a
```

**Optimization Actions:**

```bash
# Cancel jobs that are no longer needed
gryvia cancel <job-name>

# Review quotas and adjust them where teams are over-provisioned
gryvia quota ml-research --budget
kubectl edit gryviaquota <quota-name>
```

---

## Capacity Planning

### Quarterly Capacity Review

**Process:**

```bash
# 1. Review current cluster capacity and node details
gryvia capacity
gryvia cluster --detailed

# 2. Review quota usage per team
gryvia quota --budget

# 3. Review queue pressure
gryvia queue

# 4. Review spending trends
gryvia cost --period month
```

Build the demand forecast, expansion plan, cost estimate and budget request
with your usual planning and procurement tools; Gryvia does not generate these yet.

**Expansion Procedure:**

```bash
# 1. Obtain budget approval and order hardware
# (External procurement process)

# 2. Install, network and join the new nodes to Kubernetes
# (Follow the New Node Onboarding checklist below)

# 3. Confirm the new nodes are Ready
kubectl get nodes

# 4. Verify Gryvia sees them and the cluster is healthy
gryvia list nodes
gryvia health gpu

# 5. Announce the new capacity to users through your usual channels
```

---

## Maintenance Procedures

### Planned Maintenance Window

**Pre-Maintenance:**

```bash
# 1. Announce the maintenance window to users (for example 7 days before)
#    through your usual channels

# 2. Send reminders (for example 24 hours before)

# 3. Review the jobs running on the affected nodes
gryvia list jobs -a
kubectl get pods -A -o wide --field-selector spec.nodeName=gpu-node-01

# 4. Check that the remaining nodes can absorb the pending work
gryvia capacity
```

**During Maintenance:**

```bash
# 1. Cordon and mark the nodes
for node in gpu-node-01 gpu-node-02 gpu-node-03 gpu-node-04; do
  gryvia maintenance start $node --reason "quarterly firmware update"
done

# 2. Drain gracefully (Eviction API; blocked pods are reported, not deleted)
for node in gpu-node-01 gpu-node-02 gpu-node-03 gpu-node-04; do
  gryvia maintenance start $node --drain --yes
done

# 2b. See which nodes are marked and for how long
gryvia maintenance list

# 3. Perform maintenance
# - Update firmware
# - Apply patches
# - Hardware upgrades
# - Network configuration

# 4. Validate nodes
gryvia health gpu
gryvia get node gpu-node-01

# 5. Uncordon nodes
for node in gpu-node-01 gpu-node-02 gpu-node-03 gpu-node-04; do
  gryvia maintenance end $node
done
```

**Post-Maintenance:**

```bash
# 1. Verify cluster health
gryvia health
gryvia cluster --detailed

# 2. Run a smoke test job
gryvia submit -f smoke-test.yaml --wait

# 3. Monitor for issues
gryvia queue --watch 30

# 4. Send a completion notice to users through your usual channels
```

---

## Incident Response

### Incident Management Workflow

**Severity Levels:**

- **P0 (Critical)**: Complete cluster outage, data loss
- **P1 (High)**: Major functionality impaired, SLA breach
- **P2 (Medium)**: Partial functionality impaired
- **P3 (Low)**: Minor issues, no immediate impact

**P0 Incident Response:**

Declaring the incident, notifying stakeholders, assembling the response team,
opening a war room and posting status updates (every 15 minutes) happen in your
incident tooling (for example PagerDuty and Slack); Gryvia does not manage incidents.

```bash
# 1. Assess the cluster
gryvia cluster --detailed
gryvia health

# 2. Check the control plane and Gryvia components
kubectl get nodes
kubectl get pods -A | grep -v Running

# 3. Review recent events
kubectl get events -A --sort-by=.lastTimestamp

# 4. Implement fix
# ... resolution steps ...

# 5. Verify resolution
gryvia health
gryvia list jobs -a

# 6. Close the incident and schedule the post-mortem in your incident tooling
```

**Post-Mortem Template:**

```markdown
# Incident Post-Mortem: [Title]

## Summary
- **Date**: <date>
- **Duration**: <hh:mm>
- **Severity**: P0
- **Impact**: <jobs affected, cost impact>

## Timeline
- <time> - Initial alert
- <time> - Incident declared
- <time> - Root cause identified
- <time> - Fix implemented
- <time> - Verified resolution

## Root Cause
[Detailed analysis]

## Resolution
[What fixed it]

## Action Items
1. [ ] Improve monitoring for X
2. [ ] Add automated recovery for Y
3. [ ] Update runbook for Z

## Lessons Learned
- What went well
- What could be improved
- What we learned
```

---

## Checklists

### New Node Onboarding

```text
☐ Physical installation complete
☐ Network cables connected (IB + Ethernet)
☐ Power verified (redundant PSUs)
☐ BIOS configured
☐ OS installed and patched
☐ GPU drivers installed
☐ NVIDIA Fabric Manager configured (NVSwitch systems; not installed by Gryvia)
☐ Kubernetes joined cluster
☐ Node labelled nvidia.com/gpu.present=true and visible via `kubectl get gryviagpunodes`
☐ Health checks passing
☐ Test job successful
☐ Added to monitoring
☐ Added to backup
☐ Documentation updated
```

### Job Troubleshooting

```text
☐ Check job status: gryvia status <name>
☐ View logs: gryvia logs <name>
☐ Check events: kubectl describe gryviaaijob <name>
☐ Verify resources available
☐ Check quota/budget: gryvia quota <team> --budget
☐ Review node health
☐ Check network connectivity
☐ Verify image exists and accessible
☐ Check for resource conflicts
☐ Review scheduling decisions
```

---

## Escalation and Alerting Template

Gryvia does not page anyone or define an escalation policy. Fill in your own, for example:

```text
Severity P0/P1: primary on-call, then engineering manager
Severity P2:    SRE team channel, then on-call
Severity P3:    support queue
```

Sample Prometheus alert rules ship in `monitoring/prometheus-rules.yaml` and `manifests/monitoring/alerts/gpu-alerts.yaml`
(for example `GPUHighTemperature`, `GPUXIDError`, `NodeGPUDown`, `TrainingJobFailed`, `QuotaExceeded`,
`BudgetExceeded`, `StorageBackendUnhealthy`, `RDMADeviceDown`). They are templates: not every metric they query is
guaranteed to exist in your cluster, and they have not been run against production data. Map your own alerts to the
playbooks above (node failure: GPU Node Failure; `BudgetExceeded`: Out of Budget; capacity: Cluster at Capacity).

---

## Useful Commands Reference

```bash
# Cluster health
gryvia health
gryvia cluster --detailed
gryvia list nodes

# Performance
gryvia status <job-name>
gryvia gpu training --job <job-name>
gryvia gpu nccl --job <job-name>
gryvia network status

# Cost
gryvia cost <team> --detailed
gryvia quota <team> --budget

# Capacity
gryvia capacity
gryvia cluster --detailed
gryvia queue

# Jobs
gryvia list jobs -a
gryvia cancel <job-name>

# Maintenance
gryvia maintenance start <node> --reason "<why>" --drain
gryvia maintenance list
gryvia maintenance end <node>
```

---

*For additional support, see [INTEGRATIONS.md](INTEGRATIONS.md) and [ADVANCED_FEATURES.md](ADVANCED_FEATURES.md)*
