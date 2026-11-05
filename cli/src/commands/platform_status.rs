//! `gryvia status` with no job: the platform report (components, workloads, per-node detail).
//!
//! This file only talks to the cluster; the model and the text live in `crate::platform` and are pure.

use std::time::{Duration, Instant};

use anyhow::{bail, Result};
use k8s_openapi::api::apps::v1::{DaemonSet, Deployment};
use k8s_openapi::api::core::v1::{Node, Pod};
use kube::api::{Api, ListParams};

use crate::client::GryviaClient;
use crate::commands::capacity::{gvk_api, phase_bucket, PhaseBucket};
use crate::output::{print_serialized, OutputFormat};
use crate::platform::{
    build_model, render, render_brief, GpuNodeInfo, Inputs, JobCounts, NodeInfo, PodInfo,
    StatusModel, WorkloadInfo, WorkloadKind,
};
use crate::ui;

pub struct Options {
    pub namespace: String,
    pub node: Option<String>,
    pub brief: bool,
    pub wait: bool,
    pub wait_timeout: Duration,
    pub output: OutputFormat,
}

/// Parse `90`, `90s`, `5m` or `1h` into a duration.
pub fn parse_duration(text: &str) -> Result<Duration> {
    let t = text.trim();
    let (digits, unit) = match t.chars().last() {
        Some(c) if c.is_ascii_alphabetic() => (&t[..t.len() - 1], c),
        _ => (t, 's'),
    };
    let n: u64 = digits.parse().map_err(|_| {
        anyhow::anyhow!("invalid duration '{text}': use a number with s, m or h, like 90s or 5m")
    })?;
    let secs = match unit {
        's' => n,
        'm' => n * 60,
        'h' => n * 3600,
        other => bail!("invalid duration unit '{other}' in '{text}': use s, m or h"),
    };
    Ok(Duration::from_secs(secs))
}

/// Read a GryviaGpuNode's JSON into what the report needs.
pub fn gpu_node_from_json(v: &serde_json::Value) -> Option<GpuNodeInfo> {
    let node_name = v
        .pointer("/spec/nodeName")
        .and_then(|x| x.as_str())
        .or_else(|| v.pointer("/metadata/name").and_then(|x| x.as_str()))?
        .to_string();
    let statuses = v
        .pointer("/status/gpuStatus")
        .and_then(|x| x.as_array())
        .cloned()
        .unwrap_or_default();
    let healthy = statuses
        .iter()
        .filter(|g| {
            g.get("health")
                .and_then(|h| h.as_str())
                .is_some_and(|h| h.eq_ignore_ascii_case("healthy"))
        })
        .count() as u32;
    Some(GpuNodeInfo {
        node_name,
        gpu_type: v
            .pointer("/spec/gpuType")
            .and_then(|x| x.as_str())
            .unwrap_or("")
            .to_string(),
        gpu_count: v
            .pointer("/spec/gpuCount")
            .and_then(|x| x.as_u64())
            .unwrap_or(0) as u32,
        phase: v
            .pointer("/status/phase")
            .and_then(|x| x.as_str())
            .unwrap_or("")
            .to_string(),
        driver: v
            .pointer("/status/driverVersion")
            .and_then(|x| x.as_str())
            .unwrap_or("")
            .to_string(),
        cuda: v
            .pointer("/status/cudaVersion")
            .and_then(|x| x.as_str())
            .unwrap_or("")
            .to_string(),
        gpus_reported: statuses.len() as u32,
        gpus_healthy: healthy,
    })
}

/// Count jobs by phase bucket, with the same grouping as the gateway and `gryvia capacity`.
pub fn count_jobs<'a>(phases: impl IntoIterator<Item = &'a str>) -> JobCounts {
    let mut counts = JobCounts::default();
    for phase in phases {
        match phase_bucket(phase) {
            PhaseBucket::Running => counts.running += 1,
            PhaseBucket::Pending => counts.pending += 1,
            PhaseBucket::Completed => counts.completed += 1,
            PhaseBucket::Failed => counts.failed += 1,
            PhaseBucket::Other => {}
        }
    }
    counts
}

fn pod_reason(pod: &Pod) -> Option<String> {
    let status = pod.status.as_ref()?;
    for cs in status.container_statuses.iter().flatten() {
        if let Some(state) = &cs.state {
            if let Some(reason) = state.waiting.as_ref().and_then(|w| w.reason.clone()) {
                return Some(reason);
            }
            if let Some(reason) = state
                .terminated
                .as_ref()
                .filter(|t| t.exit_code != 0)
                .and_then(|t| t.reason.clone())
            {
                return Some(reason);
            }
        }
    }
    status.reason.clone()
}

fn pod_info(pod: &Pod) -> PodInfo {
    let status = pod.status.as_ref();
    PodInfo {
        name: pod.metadata.name.clone().unwrap_or_default(),
        node: pod.spec.as_ref().and_then(|s| s.node_name.clone()),
        phase: status
            .and_then(|s| s.phase.clone())
            .unwrap_or_else(|| "Unknown".to_string()),
        ready: status
            .and_then(|s| s.conditions.as_ref())
            .is_some_and(|c| c.iter().any(|c| c.type_ == "Ready" && c.status == "True")),
        reason: pod_reason(pod),
    }
}

fn node_info(node: &Node) -> NodeInfo {
    let annotations = node.metadata.annotations.as_ref();
    let maintenance = annotations.and_then(|a| {
        a.get("gryvia.io/maintenance-reason").cloned().or_else(|| {
            a.get("gryvia.io/maintenance")
                .map(|_| "maintenance".to_string())
        })
    });
    NodeInfo {
        name: node.metadata.name.clone().unwrap_or_default(),
        ready: node
            .status
            .as_ref()
            .and_then(|s| s.conditions.as_ref())
            .is_some_and(|c| c.iter().any(|c| c.type_ == "Ready" && c.status == "True")),
        unschedulable: node
            .spec
            .as_ref()
            .and_then(|s| s.unschedulable)
            .unwrap_or(false),
        maintenance,
    }
}

fn images(spec: Option<&k8s_openapi::api::core::v1::PodSpec>) -> Vec<String> {
    spec.map(|p| {
        p.containers
            .iter()
            .filter_map(|c| c.image.clone())
            .collect()
    })
    .unwrap_or_default()
}

/// Read everything the report needs. A failed read becomes a warning, never an error.

fn detect_scheduler_backend(pod: Option<&k8s_openapi::api::core::v1::PodSpec>) -> Option<String> {
    let pod = pod?;
    for c in &pod.containers {
        let Some(args) = c.args.as_ref() else { continue };
        for a in args {
            if a.contains("kai-scheduler") {
                return Some("kai".into());
            }
            if a.contains("kueue") && (a.contains("true") || a.ends_with("kueue")) {
                return Some("kueue".into());
            }
            if a.contains("native-queue") || a.contains("native-scheduler") {
                return Some("native".into());
            }
            if a.contains("volcano") {
                return Some("volcano".into());
            }
        }
    }
    None
}

pub async fn collect(client: &GryviaClient, namespace: &str) -> Inputs {
    let mut inputs = Inputs {
        namespace: namespace.to_string(),
        ..Inputs::default()
    };
    let lp = ListParams::default();

    let deployments: Api<Deployment> = Api::namespaced(client.kube_client.clone(), namespace);
    match deployments.list(&lp).await {
        Ok(list) => {
            for d in list.items {
                let spec = d.spec.as_ref();
                let status = d.status.as_ref();
                let name = d.metadata.name.clone().unwrap_or_default();
                inputs.workloads.push(WorkloadInfo {
                    kind: WorkloadKind::Deployment,
                    name: name.clone(),
                    desired: spec.and_then(|s| s.replicas).unwrap_or(1),
                    ready: status.and_then(|s| s.ready_replicas).unwrap_or(0),
                    available: status.and_then(|s| s.available_replicas).unwrap_or(0),
                    images: images(spec.and_then(|s| s.template.spec.as_ref())),
                });
                if name.contains("ai-operator") {
                    if let Some(backend) = detect_scheduler_backend(spec.and_then(|s| s.template.spec.as_ref())) {
                        inputs.scheduler_backend = backend;
                    }
                }
            }
        }
        Err(e) => inputs.warnings.push(format!(
            "could not list deployments in {namespace}: {}",
            crate::display::cluster_error_text(&e.to_string())
        )),
    }

    let daemonsets: Api<DaemonSet> = Api::namespaced(client.kube_client.clone(), namespace);
    match daemonsets.list(&lp).await {
        Ok(list) => {
            for d in list.items {
                let status = d.status.as_ref();
                inputs.workloads.push(WorkloadInfo {
                    kind: WorkloadKind::DaemonSet,
                    name: d.metadata.name.clone().unwrap_or_default(),
                    desired: status.map(|s| s.desired_number_scheduled).unwrap_or(0),
                    ready: status.map(|s| s.number_ready).unwrap_or(0),
                    available: status.and_then(|s| s.number_available).unwrap_or(0),
                    images: images(d.spec.as_ref().and_then(|s| s.template.spec.as_ref())),
                });
            }
        }
        Err(e) => inputs.warnings.push(format!(
            "could not list daemonsets in {namespace}: {}",
            crate::display::cluster_error_text(&e.to_string())
        )),
    }

    let pods: Api<Pod> = Api::namespaced(client.kube_client.clone(), namespace);
    match pods.list(&lp).await {
        Ok(list) => inputs.pods = list.items.iter().map(pod_info).collect(),
        Err(e) => inputs.warnings.push(format!(
            "could not list pods in {namespace}: {}",
            crate::display::cluster_error_text(&e.to_string())
        )),
    }

    let nodes: Api<Node> = Api::all(client.kube_client.clone());
    match nodes.list(&lp).await {
        Ok(list) => inputs.nodes = list.items.iter().map(node_info).collect(),
        Err(e) => inputs.warnings.push(format!(
            "could not list nodes: {}",
            crate::display::cluster_error_text(&e.to_string())
        )),
    }

    match gvk_api(client, "GryviaGpuNode", "gryviagpunodes")
        .list(&lp)
        .await
    {
        Ok(list) => {
            inputs.gpu_nodes = list
                .items
                .iter()
                .filter_map(|o| serde_json::to_value(o).ok())
                .filter_map(|v| gpu_node_from_json(&v))
                .collect();
        }
        Err(e) => {
            inputs.gpu_nodes_failed = true;
            inputs.warnings.push(format!(
                "could not list GPU nodes: {}",
                crate::display::cluster_error_text(&e.to_string())
            ));
        }
    }

    match gvk_api(client, "GryviaAIJob", "gryviaaijobs")
        .list(&lp)
        .await
    {
        Ok(list) => {
            let values: Vec<serde_json::Value> = list
                .items
                .iter()
                .filter_map(|o| serde_json::to_value(o).ok())
                .collect();
            inputs.jobs = count_jobs(values.iter().map(|v| {
                v.pointer("/status/phase")
                    .and_then(|p| p.as_str())
                    .unwrap_or("")
            }));
        }
        Err(e) => {
            inputs.jobs_failed = true;
            inputs.warnings.push(format!(
                "could not list jobs: {}",
                crate::display::cluster_error_text(&e.to_string())
            ));
        }
    }
    inputs
}

async fn snapshot(client: &GryviaClient, opts: &Options) -> StatusModel {
    let mut model = build_model(&collect(client, &opts.namespace).await);
    if let Some(node) = &opts.node {
        model.only_node(node);
    }
    model
}

/// Run the report. Returns the process exit code: 0 when healthy, 1 when a required component failed.
pub async fn execute(client: &GryviaClient, opts: Options) -> Result<i32> {
    let started = Instant::now();
    let mut model = snapshot(client, &opts).await;
    if opts.wait {
        while model.failed() && started.elapsed() < opts.wait_timeout {
            eprintln!(
                "{} waiting for Gryvia to become ready ({}s)...",
                ui::Marker::Warn.mark(),
                started.elapsed().as_secs()
            );
            tokio::time::sleep(Duration::from_secs(2)).await;
            model = snapshot(client, &opts).await;
        }
    }

    match opts.output {
        OutputFormat::Table => {
            let color = ui::color_enabled();
            if opts.brief {
                print!("{}", render_brief(&model, color));
            } else {
                print!("{}", render(&model, color));
            }
        }
        other => print_serialized(other.as_str(), &model)?,
    }
    Ok(if model.failed() { 1 } else { 0 })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn durations_parse() {
        assert_eq!(parse_duration("90").unwrap(), Duration::from_secs(90));
        assert_eq!(parse_duration("90s").unwrap(), Duration::from_secs(90));
        assert_eq!(parse_duration("5m").unwrap(), Duration::from_secs(300));
        assert_eq!(parse_duration("2h").unwrap(), Duration::from_secs(7200));
        assert!(parse_duration("soon").is_err());
        assert!(parse_duration("5d").is_err());
        assert!(parse_duration("").is_err());
    }

    #[test]
    fn a_gpu_node_is_read_from_its_json() {
        let v = json!({
            "metadata": {"name": "demo"},
            "spec": {"nodeName": "gpu-1", "gpuType": "H100", "gpuCount": 8},
            "status": {"phase": "Ready", "driverVersion": "550.54", "cudaVersion": "12.4",
                       "gpuStatus": [{"health": "Healthy"}, {"health": "Healthy"}, {"health": "Degraded"}]}
        });
        let g = gpu_node_from_json(&v).unwrap();
        assert_eq!(g.node_name, "gpu-1");
        assert_eq!((g.gpu_type.as_str(), g.gpu_count), ("H100", 8));
        assert_eq!((g.gpus_reported, g.gpus_healthy), (3, 2));
        assert_eq!(g.driver, "550.54");
    }

    #[test]
    fn a_gpu_node_without_status_still_parses() {
        let g = gpu_node_from_json(
            &json!({"metadata": {"name": "n"}, "spec": {"gpuType": "T4", "gpuCount": 1}}),
        )
        .unwrap();
        assert_eq!(g.node_name, "n");
        assert_eq!((g.phase.as_str(), g.gpus_reported), ("", 0));
        assert!(gpu_node_from_json(&json!({})).is_none());
    }

    #[test]
    fn jobs_are_counted_like_the_gateway() {
        let c = count_jobs([
            "Running",
            "running",
            "Scheduling",
            "Queued",
            "Pending",
            "Succeeded",
            "Completed",
            "failed",
            "weird",
            "",
        ]);
        assert_eq!(
            c,
            JobCounts {
                running: 2,
                pending: 3,
                completed: 2,
                failed: 1
            }
        );
    }
}
