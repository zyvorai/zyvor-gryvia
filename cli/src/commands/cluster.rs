// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use kube::api::{Api, ListParams};
use tokio::time::{sleep, Duration};

use crate::client::GryviaClient;
use crate::display;
use crate::types::*;
use crate::ui::{self, Cell2, Marker};

pub async fn execute(
    client: &GryviaClient,
    detailed: bool,
    watch: Option<u64>,
    output: &str,
) -> Result<()> {
    if let Some(interval) = watch {
        if interval == 0 {
            anyhow::bail!("Watch interval must be greater than 0");
        }
        if output != "table" {
            anyhow::bail!("--watch needs the table output format");
        }
        loop {
            // Clear screen using ANSI escape sequences. This works on POSIX-compliant
            // terminals but may render as garbage on non-ANSI terminals (e.g. Windows cmd.exe
            // without virtual terminal processing enabled).
            print!("\x1B[2J\x1B[1;1H");
            if let Err(e) = show_cluster_overview(client, detailed, output).await {
                eprintln!("Error refreshing cluster overview: {}", e);
            }
            sleep(Duration::from_secs(interval)).await;
        }
    } else {
        show_cluster_overview(client, detailed, output).await?;
    }

    Ok(())
}

async fn show_cluster_overview(client: &GryviaClient, detailed: bool, output: &str) -> Result<()> {
    let nodes_api: Api<GryviaGpuNode> = Api::all(client.kube_client.clone());
    let jobs_api: Api<GryviaAIJob> = Api::all(client.kube_client.clone());

    let nodes = nodes_api
        .list(&ListParams::default())
        .await
        .context("Failed to list GPU nodes")?;

    let jobs = jobs_api
        .list(&ListParams::default())
        .await
        .context("Failed to list jobs")?;

    if output != "table" {
        let doc = serde_json::json!({ "nodes": nodes.items, "jobs": jobs.items });
        return crate::output::print_serialized(output, &doc);
    }

    display::print_cluster_overview(&nodes.items, &jobs.items);

    if detailed {
        show_detailed_nodes(&nodes.items)?;
        show_detailed_jobs(&jobs.items)?;
    }

    Ok(())
}

fn show_detailed_nodes(nodes: &[GryviaGpuNode]) -> Result<()> {
    for line in node_details_lines(nodes, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

fn show_detailed_jobs(jobs: &[GryviaAIJob]) -> Result<()> {
    for line in job_details_lines(jobs, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

/// Temperature marker: 90C and above is an error, 80C and above a warning.
fn temperature_marker(celsius: i64) -> Marker {
    if celsius >= 90 {
        Marker::Error
    } else if celsius >= 80 {
        Marker::Warn
    } else {
        Marker::Ok
    }
}

/// `--detailed` node section: the node table, then one table of GPUs per node that reports any.
pub fn node_details_lines(nodes: &[GryviaGpuNode], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("GPU Node Details", color), String::new()];
    if nodes.is_empty() {
        lines.push(format!(
            "{} No GPU nodes registered",
            Marker::Unknown.paint_with("ℹ", color)
        ));
        lines.push(String::new());
        return lines;
    }
    lines.extend(display::nodes_lines(nodes, color));
    lines.push(String::new());

    for node in nodes {
        let status = node.status.clone().unwrap_or_default();
        if status.gpu_status.is_empty() {
            continue;
        }
        let versions = match (status.driver_version.as_str(), status.cuda_version.as_str()) {
            ("", "") => String::new(),
            (d, "") => format!("  driver {d}"),
            ("", c) => format!("  CUDA {c}"),
            (d, c) => format!("  driver {d}, CUDA {c}"),
        };
        lines.push(format!(
            "{}{}",
            ui::section(&node.spec.node_name, color),
            ui::ansi(&versions, "2", color)
        ));
        let rows: Vec<Vec<Cell2>> = status
            .gpu_status
            .iter()
            .map(|gpu| {
                let health = Marker::from_phase(&gpu.health);
                vec![
                    (gpu.index.to_string(), None),
                    (gpu.uuid.chars().take(12).collect::<String>(), None),
                    (gpu.health.clone(), Some(health)),
                    (
                        format!("{}C", gpu.temperature),
                        Some(temperature_marker(gpu.temperature)),
                    ),
                    (format!("{}%", gpu.utilization), None),
                    (format!("{}MB", gpu.memory_used), None),
                    (format!("{}MB", gpu.memory_total), None),
                ]
            })
            .collect();
        for line in ui::grid(
            &[
                "GPU",
                "UUID",
                "HEALTH",
                "TEMP",
                "UTIL",
                "MEM USED",
                "MEM TOTAL",
            ],
            &rows,
            color,
        ) {
            lines.push(format!("  {line}"));
        }
        lines.push(String::new());
    }
    lines
}

/// `--detailed` job section: the running jobs.
pub fn job_details_lines(jobs: &[GryviaAIJob], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("Job Details", color), String::new()];
    let running: Vec<&GryviaAIJob> = jobs
        .iter()
        .filter(|j| j.status.as_ref().map(|s| s.phase.as_str()) == Some("Running"))
        .collect();
    if running.is_empty() {
        lines.push(format!(
            "{} No running jobs",
            Marker::Unknown.paint_with("ℹ", color)
        ));
        lines.push(String::new());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = running
        .iter()
        .map(|job| {
            let distributed = if job.spec.distributed.enabled {
                format!(
                    "{} ({}x{})",
                    job.spec.distributed.framework,
                    job.spec.distributed.nodes,
                    job.spec.distributed.gpus_per_node
                )
            } else {
                "no".to_string()
            };
            let runtime = match job.status.as_ref().and_then(|s| s.start_time) {
                Some(start) => {
                    let d = chrono::Utc::now().signed_duration_since(start);
                    if d.num_hours() > 0 {
                        format!("{}h{}m", d.num_hours(), d.num_minutes() % 60)
                    } else {
                        format!("{}m", d.num_minutes())
                    }
                }
                None => "-".to_string(),
            };
            vec![
                (
                    job.metadata
                        .name
                        .clone()
                        .unwrap_or_else(|| "<unknown>".into()),
                    None,
                ),
                (job.spec.job_type.clone(), None),
                (job.spec.gpus.to_string(), None),
                (job.spec.gpu_type.clone(), None),
                (distributed, None),
                (runtime, None),
            ]
        })
        .collect();
    for line in ui::grid(
        &["NAME", "TYPE", "GPUS", "GPU TYPE", "DISTRIBUTED", "RUNTIME"],
        &rows,
        color,
    ) {
        lines.push(line);
    }
    lines.push(String::new());
    lines
}

#[cfg(test)]
mod tests {
    use super::*;

    fn node_with_gpus() -> GryviaGpuNode {
        serde_json::from_value(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaGpuNode",
            "metadata": {"name": "n"},
            "spec": {"nodeName": "gpu-1", "gpuType": "H100", "gpuCount": 2, "memoryGB": 80},
            "status": {"phase": "Ready", "driverVersion": "550.54", "cudaVersion": "12.4",
                "gpuStatus": [
                    {"index": 0, "uuid": "GPU-1234567890abcdef", "health": "Healthy", "temperature": 62, "utilization": 71, "memoryUsed": 41000, "memoryTotal": 81920},
                    {"index": 1, "uuid": "GPU-fedcba0987654321", "health": "Degraded", "temperature": 91, "utilization": 5, "memoryUsed": 100, "memoryTotal": 81920}
                ]}
        }))
        .unwrap()
    }

    #[test]
    fn temperature_thresholds() {
        assert_eq!(temperature_marker(62), Marker::Ok);
        assert_eq!(temperature_marker(80), Marker::Warn);
        assert_eq!(temperature_marker(90), Marker::Error);
    }

    #[test]
    fn node_details_show_each_gpu_from_the_real_status_fields() {
        let text = node_details_lines(&[node_with_gpus()], false).join("\n");
        assert!(text.contains("━━━ GPU Node Details ━━━"));
        assert!(text.contains("gpu-1  driver 550.54, CUDA 12.4"));
        assert!(text.contains("GPU  UUID          HEALTH    TEMP  UTIL  MEM USED  MEM TOTAL"));
        assert!(text.contains("0    GPU-12345678  Healthy   62C   71%   41000MB   81920MB"));
        assert!(text.contains("1    GPU-fedcba09  Degraded  91C   5%    100MB     81920MB"));
    }

    #[test]
    fn empty_sections_say_so() {
        assert!(node_details_lines(&[], false)
            .join("\n")
            .contains("No GPU nodes registered"));
        assert!(job_details_lines(&[], false)
            .join("\n")
            .contains("No running jobs"));
    }
}
