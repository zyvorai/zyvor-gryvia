# Volcano scheduling backend

[Volcano](https://volcano.sh/) is a CNCF batch scheduler for Kubernetes (gangs, queues, DRF, framework plugins).
Gryvia can hand `GryviaAIJob` pods to Volcano the same way it hands them to KAI: set `schedulerName` and a queue
annotation, keep lifecycle/tenants/metering in Gryvia.

**Status:** opt-in stub. Sets `schedulerName: volcano` and `scheduling.volcano.sh/queue-name`. Does **not** create
Volcano `PodGroup` / `vcjob` objects or translate framework plugins. Install Volcano yourself. Untested on GPUs.
Mutually exclusive with KAI, Kueue and the native queue gate.

## Turn it on

```bash
# Install Volcano per https://volcano.sh/en/docs/installation/
helm upgrade --install gryvia ./helm/gryvia -n gryvia-system \
  --set aiOperator.volcanoScheduler=true \
  --set aiOperator.volcanoDefaultQueue=default
```

Jobs with `spec.queueName` (or `gryvia.io/queue-name`) map to that Volcano queue; otherwise `volcanoDefaultQueue`.

## Why not a full vcjob bridge?

Volcano’s strength is mature gang/DRF and Spark/MPI plugins. Gryvia already covers gangs through Kueue and KAI.
This backend is for clusters that already standardize on Volcano as the pod scheduler. A future wave can add
PodGroup minMember mirroring if demand justifies it (see [competitors](competitors.md)).
