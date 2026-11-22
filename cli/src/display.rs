// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use crate::commands::capacity::{phase_bucket, PhaseBucket};
use crate::types::*;
use crate::ui::{self, Cell2, Marker};
use colored::*;

fn phase_cell(phase: &str) -> Cell2 {
    let marker = Marker::from_phase(phase);
    let text = if phase.is_empty() { "-" } else { phase };
    (text.to_string(), Some(marker))
}

fn print_lines(lines: Vec<String>) {
    for line in lines {
        println!("{line}");
    }
}

/// Lines of the jobs table (NAME TYPE GPUS GPU TYPE STATUS AGE).
pub fn jobs_lines(jobs: &[GryviaAIJob], color: bool) -> Vec<String> {
    let rows: Vec<Vec<Cell2>> = jobs
        .iter()
        .map(|job| {
            let status = job.status.clone().unwrap_or_default();
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
                phase_cell(&status.phase),
                (format_age(job.metadata.creation_timestamp.as_ref()), None),
            ]
        })
        .collect();
    ui::grid(
        &["NAME", "TYPE", "GPUS", "GPU TYPE", "STATUS", "AGE"],
        &rows,
        color,
    )
}

pub fn print_jobs_table(jobs: &[GryviaAIJob]) {
    print_lines(jobs_lines(jobs, ui::color_enabled()));
}

/// Lines of the quotas table.
pub fn quotas_lines(quotas: &[GryviaQuota], color: bool) -> Vec<String> {
    let rows: Vec<Vec<Cell2>> = quotas
        .iter()
        .map(|quota| {
            let status = quota.status.clone().unwrap_or_default();
            let max = quota.spec.gpu_quota.max_gpus;
            let budget = match status.budget_status {
                Some(ref b) => format!(
                    "${:.0}/{:.0}",
                    b.spent_this_month,
                    quota
                        .spec
                        .budget
                        .as_ref()
                        .map(|b| b.monthly_budget)
                        .unwrap_or(0.0)
                ),
                None => "N/A".to_string(),
            };
            vec![
                (quota.spec.team.clone(), None),
                (
                    format!("{}/{}", status.current_usage.allocated_gpus, max),
                    None,
                ),
                (max.to_string(), None),
                (status.current_usage.running_jobs.to_string(), None),
                (status.current_usage.queued_jobs.to_string(), None),
                (budget, None),
                phase_cell(&status.phase),
            ]
        })
        .collect();
    ui::grid(
        &[
            "TEAM",
            "ALLOCATED",
            "MAX GPUS",
            "RUNNING",
            "QUEUED",
            "BUDGET",
            "STATUS",
        ],
        &rows,
        color,
    )
}

pub fn print_quotas_table(quotas: &[GryviaQuota]) {
    print_lines(quotas_lines(quotas, ui::color_enabled()));
}

/// Lines of the GPU nodes table.
pub fn nodes_lines(nodes: &[GryviaGpuNode], color: bool) -> Vec<String> {
    let rows: Vec<Vec<Cell2>> = nodes
        .iter()
        .map(|node| {
            let status = node.status.clone().unwrap_or_default();
            let memory = if node.spec.memory_gb > 0 {
                format!("{} GB", node.spec.memory_gb)
            } else {
                "-".to_string()
            };
            let rdma = if node.spec.rdma {
                Marker::Ok
            } else {
                Marker::Disabled
            };
            vec![
                (node.spec.node_name.clone(), None),
                (node.spec.gpu_type.clone(), None),
                (node.spec.gpu_count.to_string(), None),
                (memory, None),
                (rdma.glyph().to_string(), Some(rdma)),
                phase_cell(&status.phase),
            ]
        })
        .collect();
    ui::grid(
        &["NODE", "GPU TYPE", "GPUS", "MEMORY", "RDMA", "STATUS"],
        &rows,
        color,
    )
}

pub fn print_nodes_table(nodes: &[GryviaGpuNode]) {
    print_lines(nodes_lines(nodes, ui::color_enabled()));
}

/// The `gryvia cluster` overview, as lines.
pub fn cluster_overview_lines(
    nodes: &[GryviaGpuNode],
    jobs: &[GryviaAIJob],
    color: bool,
) -> Vec<String> {
    let mut by_type: std::collections::BTreeMap<&str, u32> = std::collections::BTreeMap::new();
    let mut total_gpus: u32 = 0;
    for node in nodes {
        *by_type.entry(node.spec.gpu_type.as_str()).or_insert(0) += node.spec.gpu_count;
        total_gpus += node.spec.gpu_count;
    }
    let mut counts = (0u32, 0u32, 0u32, 0u32); // running, pending, completed, failed
    let mut allocated: u32 = 0;
    for job in jobs {
        let phase = job.status.clone().unwrap_or_default().phase;
        match phase_bucket(&phase) {
            PhaseBucket::Running => {
                counts.0 += 1;
                allocated += job.spec.gpus;
            }
            PhaseBucket::Pending => counts.1 += 1,
            PhaseBucket::Completed => counts.2 += 1,
            PhaseBucket::Failed => counts.3 += 1,
            PhaseBucket::Other => {}
        }
    }
    let free = total_gpus.saturating_sub(allocated);
    let free_marker = if total_gpus == 0 {
        Marker::Disabled
    } else if free == 0 {
        Marker::Warn
    } else {
        Marker::Ok
    };

    let mut lines = vec![ui::header("GPU CLUSTER OVERVIEW", color), String::new()];
    lines.push(ui::kv("Nodes:", &nodes.len().to_string(), 18, color));
    lines.push(ui::kv("Total GPUs:", &total_gpus.to_string(), 18, color));
    lines.push(ui::kv("Allocated GPUs:", &allocated.to_string(), 18, color));
    lines.push(ui::kv(
        "Free GPUs:",
        &free_marker.paint_with(&free.to_string(), color),
        18,
        color,
    ));
    lines.push(String::new());

    if !by_type.is_empty() {
        lines.push(ui::section("GPU types", color));
        let rows: Vec<Vec<Cell2>> = by_type
            .iter()
            .map(|(t, n)| vec![(t.to_string(), None), (n.to_string(), None)])
            .collect();
        for line in ui::grid(&["TYPE", "GPUS"], &rows, color) {
            lines.push(format!("  {line}"));
        }
        lines.push(String::new());
    }

    lines.push(ui::section("Jobs", color));
    let job_rows = vec![vec![
        (counts.0.to_string(), Some(Marker::Ok)),
        (counts.1.to_string(), Some(Marker::Warn)),
        (counts.2.to_string(), None),
        (
            counts.3.to_string(),
            Some(if counts.3 > 0 {
                Marker::Error
            } else {
                Marker::Disabled
            }),
        ),
    ]];
    for line in ui::grid(
        &["RUNNING", "PENDING", "COMPLETED", "FAILED"],
        &job_rows,
        color,
    ) {
        lines.push(format!("  {line}"));
    }
    lines.push(String::new());
    lines
}

pub fn print_cluster_overview(nodes: &[GryviaGpuNode], jobs: &[GryviaAIJob]) {
    print_lines(cluster_overview_lines(nodes, jobs, ui::color_enabled()));
}

/// Time elapsed since a Kubernetes timestamp.
pub fn age_since(ts: &k8s_openapi::apimachinery::pkg::apis::meta::v1::Time) -> chrono::Duration {
    chrono::Utc::now().signed_duration_since(
        chrono::DateTime::from_timestamp(ts.0.as_second(), 0).unwrap_or_default(),
    )
}

fn format_age(timestamp: Option<&k8s_openapi::apimachinery::pkg::apis::meta::v1::Time>) -> String {
    if let Some(ts) = timestamp {
        let age = age_since(ts);

        if age.num_seconds() < 0 {
            "clock skew".to_string()
        } else if age.num_days() > 0 {
            format!("{}d", age.num_days())
        } else if age.num_hours() > 0 {
            format!("{}h", age.num_hours())
        } else if age.num_minutes() > 0 {
            format!("{}m", age.num_minutes())
        } else {
            format!("{}s", age.num_seconds())
        }
    } else {
        "-".to_string()
    }
}

/// Plain-language text for common cluster API failures (a missing API or a permission problem), or the
/// first line of the raw message otherwise.
pub fn cluster_error_text(raw: &str) -> String {
    let lower = raw.to_ascii_lowercase();
    if lower.contains("404 page not found")
        || lower.contains("could not find the requested resource")
    {
        "the gryvia.io/v1alpha1 API is not available on this cluster (CRDs missing or an older version)"
            .to_string()
    } else if lower.contains("forbidden") {
        "permission denied by the cluster (RBAC)".to_string()
    } else {
        raw.lines().next().unwrap_or("").chars().take(160).collect()
    }
}

/// A fatal error as text: the top message, its causes (deduplicated), and a hint for common problems.
pub fn fatal_lines(err: &anyhow::Error, color: bool) -> Vec<String> {
    let mut messages: Vec<String> = Vec::new();
    let api_missing = err.chain().any(|c| {
        let t = c.to_string().to_ascii_lowercase();
        t.contains("404 page not found") || t.contains("could not find the requested resource")
    });
    for cause in err.chain() {
        let text = cause.to_string();
        if api_missing
            && (text.contains("ApiError")
                || text.contains("404 page not found")
                || text.contains("Failed to parse error data"))
        {
            continue;
        }
        // Long, nested library messages repeat their cause; keep each distinct one, trimmed.
        let text: String = text.chars().take(220).collect();
        if !messages.iter().any(|m| m == &text) {
            messages.push(text);
        }
    }
    if api_missing {
        messages.push(cluster_error_text("404 page not found"));
    }
    let mut lines = vec![format!(
        "{} {}",
        Marker::Error.paint_with(Marker::Error.glyph(), color),
        ui::ansi(&messages[0], "1", color)
    )];
    for cause in messages.iter().skip(1).take(2) {
        lines.push(ui::ansi(&format!("  caused by: {cause}"), "2", color));
    }
    let all = messages.join(" ").to_ascii_lowercase();
    let hint = if api_missing {
        Some("install or update the CRDs (kubectl apply -f crds/, or helm upgrade gryvia); older installs used gryvia.io/v1")
    } else if all.contains("kubeconfig") || all.contains("kubernetes config") {
        Some("point KUBECONFIG at your cluster, or pass --context <name>")
    } else if all.contains("connection refused")
        || all.contains("error trying to connect")
        || all.contains("timed out")
    {
        Some("check that the cluster is reachable (kubectl get nodes)")
    } else if all.contains("forbidden") || all.contains("unauthorized") {
        Some("your kubeconfig user lacks permission for this resource")
    } else {
        None
    };
    if let Some(h) = hint {
        lines.push(format!("  {} {h}", ui::ansi("hint:", "1;33", color)));
    }
    lines
}

pub fn print_success(message: &str) {
    println!("{} {}", "✓".green().bold(), message);
}

pub fn print_error(message: &str) {
    eprintln!("{} {}", "✗".red().bold(), message);
}

pub fn print_warning(message: &str) {
    println!("{} {}", "⚠".yellow().bold(), message);
}

pub fn print_info(message: &str) {
    println!("{} {}", "ℹ".cyan().bold(), message);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn job(name: &str, phase: &str, gpus: u32) -> GryviaAIJob {
        serde_json::from_value(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaAIJob",
            "metadata": {"name": name},
            "spec": {"type": "training", "gpus": gpus, "gpuType": "H100", "image": "x"},
            "status": {"phase": phase}
        }))
        .unwrap()
    }

    fn gpu_node(name: &str, count: u32, phase: &str) -> GryviaGpuNode {
        serde_json::from_value(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaGpuNode",
            "metadata": {"name": name},
            "spec": {"nodeName": name, "gpuType": "H100", "gpuCount": count, "memoryGB": 80, "rdma": true},
            "status": {"phase": phase}
        }))
        .unwrap()
    }

    #[test]
    fn jobs_table_is_aligned_and_marked() {
        let lines = jobs_lines(
            &[job("train", "Running", 2), job("etl", "Failed", 1)],
            false,
        );
        assert_eq!(lines[0], "NAME   TYPE      GPUS  GPU TYPE  STATUS   AGE");
        assert!(lines[1].starts_with("train  training  2     H100      Running"));
        assert!(lines[2].contains("Failed"));
        let colored = jobs_lines(&[job("etl", "Failed", 1)], true);
        assert!(colored[1].contains("\x1b[1;31mFailed"));
    }

    #[test]
    fn nodes_table_uses_the_real_fields() {
        let lines = nodes_lines(&[gpu_node("gpu-1", 8, "Ready")], false);
        assert_eq!(lines[0], "NODE   GPU TYPE  GPUS  MEMORY  RDMA  STATUS");
        assert!(lines[1].starts_with("gpu-1  H100      8     80 GB   ✓     Ready"));
    }

    #[test]
    fn overview_counts_gpus_and_jobs() {
        let text = cluster_overview_lines(
            &[gpu_node("a", 8, "Ready"), gpu_node("b", 4, "Ready")],
            &[
                job("r", "Running", 2),
                job("p", "Pending", 4),
                job("d", "Succeeded", 1),
                job("f", "Failed", 1),
            ],
            false,
        )
        .join("\n");
        assert!(text.contains("Total GPUs:       12"));
        assert!(text.contains("Allocated GPUs:   2"));
        assert!(text.contains("Free GPUs:        10"));
        assert!(text.contains("H100  12"));
        assert!(text.contains("RUNNING  PENDING  COMPLETED  FAILED"));
        assert!(text.contains("1        1        1          1"));
    }

    #[test]
    fn fatal_errors_are_short_and_carry_a_hint() {
        let err = anyhow::anyhow!("failed to read kubeconfig from '/nope': No such file")
            .context("Failed to infer Kubernetes config");
        let lines = fatal_lines(&err, false);
        assert_eq!(lines[0], "✗ Failed to infer Kubernetes config");
        assert!(lines[1].starts_with("  caused by: failed to read kubeconfig"));
        assert!(lines.last().unwrap().contains("hint: point KUBECONFIG"));
        assert!(lines.len() <= 4);
        let plain = fatal_lines(&anyhow::anyhow!("boom"), false);
        assert_eq!(plain, vec!["✗ boom"]);
        assert!(fatal_lines(&anyhow::anyhow!("boom"), true)[0].contains("\x1b[1;31m"));
    }

    #[test]
    fn a_missing_api_is_explained_not_dumped() {
        let raw =
            "ApiError: 404 page not found\n: Failed to parse error data (Status { code: 404 })";
        let err = anyhow::anyhow!(raw.to_string()).context("Failed to list GPU nodes");
        let lines = fatal_lines(&err, false);
        assert_eq!(lines[0], "✗ Failed to list GPU nodes");
        assert_eq!(
            lines[1],
            "  caused by: the gryvia.io/v1alpha1 API is not available on this cluster (CRDs missing or an older version)"
        );
        assert!(lines.last().unwrap().contains("kubectl apply -f crds/"));
        assert!(!lines.join("\n").contains("Status {"));
    }

    #[test]
    fn cluster_error_text_is_plain() {
        assert!(cluster_error_text("ApiError: 404 page not found").contains("not available"));
        assert!(cluster_error_text("pods is forbidden: User x").contains("permission denied"));
        assert_eq!(cluster_error_text("boom\nsecond line"), "boom");
    }

    #[test]
    fn format_age_without_timestamp_is_a_dash() {
        assert_eq!(format_age(None), "-");
    }
}
