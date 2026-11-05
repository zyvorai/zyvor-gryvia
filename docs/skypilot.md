# SkyPilot on Gryvia

[SkyPilot](https://skypilot.readthedocs.io/) launches portable AI jobs across clouds and Kubernetes.
Gryvia does **not** broker multi-cloud capacity. Use SkyPilot’s Kubernetes backend against a cluster
that already runs Gryvia when you want SkyPilot’s YAML UX and autostop on top of Gryvia’s scheduling
and metering.

## Export a task from a Gryvia job

```bash
gryvia export skypilot my-train-job > train.sky.yaml
sky launch -c gryvia-k8s train.sky.yaml
```

The generated YAML sets `resources.cloud: kubernetes`, maps `spec.gpus` / `gpuType` to `accelerators`,
and copies `spec.command` into `run:`. Review and edit before launch.

## Idle reclaim

- SkyPilot: `autostop` on the cluster/VM.
- Gryvia jobs: `spec.idleTimeout` ([idle-autostop](idle-autostop.md)).
- Gryvia inference: `scaleToZero`.

See [competitors](competitors.md).
