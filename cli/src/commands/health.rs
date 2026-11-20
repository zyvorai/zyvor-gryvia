// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

use anyhow::{Context, Result};
use kube::api::{Api, ApiResource, GroupVersionKind, ListParams};
use kube::core::DynamicObject;

use crate::client::GryviaClient;
use crate::types::*;
use crate::ui::{self, Cell2, Marker};

fn print_lines(lines: Vec<String>) {
    for line in lines {
        println!("{line}");
    }
}

/// Marker for a health check phase. Unlike `Marker::from_phase`, an unrecognised phase is an error.
fn check_marker(phase: &str) -> Marker {
    match phase.trim().to_ascii_lowercase().as_str() {
        "healthy" | "ready" | "configured" => Marker::Ok,
        "degraded" | "configuring" | "pending" | "partiallyconfigured" => Marker::Warn,
        _ => Marker::Error,
    }
}

fn empty_note(message: &str, color: bool) -> Vec<String> {
    vec![
        format!("{} {message}", Marker::Unknown.paint_with("ℹ", color)),
        String::new(),
    ]
}

fn str_at<'a>(v: Option<&'a serde_json::Value>, key: &str) -> Option<&'a str> {
    v.and_then(|s| s.get(key)).and_then(|v| v.as_str())
}

/// GPU node health section.
pub fn gpu_health_lines(nodes: &[GryviaGpuNode], color: bool) -> Vec<String> {
    let mut lines = vec![ui::section("GPU Nodes", color)];
    if nodes.is_empty() {
        lines.extend(empty_note("No GPU nodes registered", color));
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = nodes
        .iter()
        .map(|node| {
            let phase = node.status.clone().unwrap_or_default().phase;
            let marker = check_marker(&phase);
            vec![
                (node.spec.node_name.clone(), None),
                (phase, Some(marker)),
                (
                    format!("{} x {}", node.spec.gpu_count, node.spec.gpu_type),
                    None,
                ),
            ]
        })
        .collect();
    lines.extend(ui::grid(&["NODE", "STATUS", "GPUs"], &rows, color));
    lines.push(String::new());
    lines
}

/// Storage health section (raw `GryviaStorage` objects).
pub fn storage_health_lines(storages: &[DynamicObject], color: bool) -> Vec<String> {
    let mut lines = vec![ui::section("Storage", color)];
    if storages.is_empty() {
        lines.extend(empty_note("No storage resources configured", color));
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = storages
        .iter()
        .map(|storage| {
            let name = storage.metadata.name.as_deref().unwrap_or("<unknown>");
            let spec = storage.data.get("spec");
            let status = storage.data.get("status");
            let rdma = spec
                .and_then(|s| s.get("rdma"))
                .and_then(|v| v.as_bool())
                .unwrap_or(false);
            let csi = status
                .and_then(|s| s.get("csiDriverInstalled"))
                .and_then(|v| v.as_bool())
                .unwrap_or(false);
            let phase = str_at(status, "phase").unwrap_or("Unknown");
            vec![
                (name.to_string(), None),
                (str_at(spec, "backend").unwrap_or("-").to_string(), None),
                (str_at(spec, "capacity").unwrap_or("-").to_string(), None),
                (
                    if rdma { "yes" } else { "no" }.to_string(),
                    if rdma { Some(Marker::Ok) } else { None },
                ),
                (
                    if csi { "installed" } else { "missing" }.to_string(),
                    Some(if csi { Marker::Ok } else { Marker::Error }),
                ),
                (phase.to_string(), Some(check_marker(phase))),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &["NAME", "BACKEND", "CAPACITY", "RDMA", "CSI", "STATUS"],
        &rows,
        color,
    ));
    lines.push(String::new());
    lines
}

/// Network health section (raw `GryviaNetwork` objects).
pub fn network_health_lines(networks: &[DynamicObject], color: bool) -> Vec<String> {
    let mut lines = vec![ui::section("Network", color)];
    if networks.is_empty() {
        lines.extend(empty_note("No network resources configured", color));
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = networks
        .iter()
        .map(|network| {
            let name = network.metadata.name.as_deref().unwrap_or("<unknown>");
            let spec = network.data.get("spec");
            let status = network.data.get("status");
            let count = |key: &str| {
                status
                    .and_then(|s| s.get(key))
                    .and_then(|v| v.as_i64())
                    .unwrap_or(0)
            };
            let phase = str_at(status, "phase").unwrap_or("Unknown");
            vec![
                (name.to_string(), None),
                (str_at(spec, "networkType").unwrap_or("-").to_string(), None),
                (
                    format!("{}/{}", count("configuredNodes"), count("totalNodes")),
                    None,
                ),
                (phase.to_string(), Some(check_marker(phase))),
            ]
        })
        .collect();
    lines.extend(ui::grid(&["NAME", "TYPE", "NODES", "STATUS"], &rows, color));
    lines.push(String::new());
    lines
}

pub async fn execute(client: &GryviaClient, component: &str) -> Result<()> {
    let color = ui::color_enabled();
    println!("{}", ui::header("Cluster Health Check", color));
    println!();

    match component {
        "all" => {
            check_gpu_health(client).await?;
            check_storage_health(client).await?;
            check_network_health(client).await?;
        }
        "gpu" => check_gpu_health(client).await?,
        "storage" => check_storage_health(client).await?,
        "network" => check_network_health(client).await?,
        _ => {
            anyhow::bail!(
                "Unknown component: {}. Valid components: all, gpu, storage, network",
                component
            );
        }
    }

    Ok(())
}

async fn check_gpu_health(client: &GryviaClient) -> Result<()> {
    let api: Api<GryviaGpuNode> = Api::all(client.kube_client.clone());
    let nodes = api
        .list(&ListParams::default())
        .await
        .context("Failed to list GPU nodes")?;
    print_lines(gpu_health_lines(&nodes.items, ui::color_enabled()));
    Ok(())
}

async fn check_storage_health(client: &GryviaClient) -> Result<()> {
    let color = ui::color_enabled();
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaStorage",
    ));
    let api: Api<DynamicObject> = Api::all_with(client.kube_client.clone(), &ar);

    match api.list(&ListParams::default()).await {
        Ok(list) => print_lines(storage_health_lines(&list.items, color)),
        Err(e) => {
            // CRD might not be installed
            println!("{}", ui::section("Storage", color));
            println!("  Could not query storage resources: {}", e);
            println!();
        }
    }
    Ok(())
}

async fn check_network_health(client: &GryviaClient) -> Result<()> {
    let color = ui::color_enabled();
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaNetwork",
    ));
    let api: Api<DynamicObject> = Api::all_with(client.kube_client.clone(), &ar);

    match api.list(&ListParams::default()).await {
        Ok(list) => print_lines(network_health_lines(&list.items, color)),
        Err(e) => {
            println!("{}", ui::section("Network", color));
            println!("  Could not query network resources: {}", e);
            println!();
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn node(name: &str, phase: &str) -> GryviaGpuNode {
        serde_json::from_value(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaGpuNode",
            "metadata": {"name": name},
            "spec": {"nodeName": name, "gpuType": "H100", "gpuCount": 8, "memoryGB": 80},
            "status": {"phase": phase}
        }))
        .unwrap()
    }

    fn dynamic(v: serde_json::Value) -> DynamicObject {
        serde_json::from_value(v).unwrap()
    }

    #[test]
    fn gpu_health_aligned() {
        let lines = gpu_health_lines(&[node("gpu-1", "Ready"), node("gpu-22", "Degraded")], false);
        assert_eq!(
            lines,
            vec![
                "GPU Nodes",
                "NODE    STATUS    GPUs",
                "gpu-1   Ready     8 x H100",
                "gpu-22  Degraded  8 x H100",
                "",
            ]
        );
    }

    #[test]
    fn empty_sections_say_so() {
        assert_eq!(
            gpu_health_lines(&[], false),
            vec!["GPU Nodes", "ℹ No GPU nodes registered", ""]
        );
        assert!(storage_health_lines(&[], false)[1].contains("No storage resources"));
        assert!(network_health_lines(&[], false)[1].contains("No network resources"));
    }

    #[test]
    fn storage_and_network_aligned() {
        let storage = dynamic(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaStorage",
            "metadata": {"name": "lustre"},
            "spec": {"backend": "lustre", "capacity": "100Ti", "rdma": true},
            "status": {"phase": "Ready", "csiDriverInstalled": false}
        }));
        assert_eq!(
            storage_health_lines(&[storage], false),
            vec![
                "Storage",
                "NAME    BACKEND  CAPACITY  RDMA  CSI      STATUS",
                "lustre  lustre   100Ti     yes   missing  Ready",
                "",
            ]
        );
        let network = dynamic(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaNetwork",
            "metadata": {"name": "ib"},
            "spec": {"networkType": "infiniband"},
            "status": {"phase": "Configuring", "configuredNodes": 3, "totalNodes": 8}
        }));
        assert_eq!(
            network_health_lines(&[network], false),
            vec![
                "Network",
                "NAME  TYPE        NODES  STATUS",
                "ib    infiniband  3/8    Configuring",
                "",
            ]
        );
    }

    #[test]
    fn markers_and_color() {
        assert_eq!(check_marker("Degraded"), Marker::Warn);
        assert_eq!(check_marker("Weird"), Marker::Error);
        let colored = gpu_health_lines(&[node("gpu-1", "Ready")], true);
        let joined = colored.join("\n");
        assert!(joined.contains("\x1b["));
        let plain = gpu_health_lines(&[node("gpu-1", "Ready")], false);
        let mut stripped = String::new();
        let mut in_esc = false;
        for c in joined.chars() {
            match (in_esc, c) {
                (false, '\x1b') => in_esc = true,
                (true, 'm') => in_esc = false,
                (false, c) => stripped.push(c),
                _ => {}
            }
        }
        assert_eq!(stripped, plain.join("\n"));
    }
}
