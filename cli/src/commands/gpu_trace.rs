// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use kube::api::{Api, ApiResource, GroupVersionKind, ListParams};
use kube::core::DynamicObject;

use crate::client::GryviaClient;
use crate::display;
use crate::gateway::GatewayClient;
use crate::ui::{self, Cell2, Marker};

pub enum GpuAction {
    Nccl {
        job: String,
        follow: bool,
        namespace: String,
    },
    Rdma {
        node: Option<String>,
        job: Option<String>,
        namespace: String,
    },
    Training {
        job: String,
        namespace: String,
    },
}

pub async fn execute(client: &GryviaClient, action: GpuAction) -> Result<()> {
    match action {
        GpuAction::Nccl {
            job,
            follow,
            namespace,
        } => execute_nccl(client, &job, follow, &namespace).await,
        GpuAction::Rdma {
            node,
            job,
            namespace,
        } => execute_rdma(client, node.as_deref(), job.as_deref(), &namespace).await,
        GpuAction::Training { job, namespace } => execute_training(client, &job, &namespace).await,
    }
}

async fn execute_nccl(
    client: &GryviaClient,
    job: &str,
    _follow: bool,
    namespace: &str,
) -> Result<()> {
    let color = ui::color_enabled();
    println!(
        "{}",
        ui::header(&format!("NCCL Collective Stats: {job}"), color)
    );
    println!();

    // Query GryviaTrainingInsight for this job
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaTrainingInsight",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let label_selector = format!("gryvia.io/job={}", job);
    let params = ListParams::default().labels(&label_selector);

    let insights = match api.list(&params).await {
        Ok(list) => list,
        Err(e) => {
            display::print_warning(&format!("Could not query training insights: {}", e));
            println!(
                "{}",
                hint(
                    "Ensure a GryviaTrainingInsight CR exists for this job.",
                    color
                )
            );
            println!();
            return Ok(());
        }
    };

    if insights.items.is_empty() {
        // Also try to get by name directly
        match api.get(job).await {
            Ok(insight) => {
                print_lines(nccl_lines(&insight, color));
            }
            Err(_) => {
                println!("{}", hint("No NCCL stats found for this job.", color));
                println!(
                    "{}",
                    hint(
                        "Create a GryviaTrainingInsight CR targeting this job to collect stats.",
                        color
                    )
                );
            }
        }
    } else {
        for insight in &insights.items {
            print_lines(nccl_lines(insight, color));
        }
    }

    println!();
    Ok(())
}

fn print_lines(lines: Vec<String>) {
    for line in lines {
        println!("{line}");
    }
}

/// A hint line: warning glyph and text, for pages whose data source is missing.
fn hint(text: &str, color: bool) -> String {
    format!(
        "{} {}",
        Marker::Warn.paint_with(Marker::Warn.glyph(), color),
        text
    )
}

fn bottleneck_marker(bottleneck: &str) -> Marker {
    match bottleneck {
        "communication" => Marker::Error,
        "compute" | "data_loading" => Marker::Warn,
        _ => Marker::Disabled,
    }
}

/// Lines of the NCCL rank table and communication analysis for one training insight.
pub fn nccl_lines(insight: &DynamicObject, color: bool) -> Vec<String> {
    let status = insight.data.get("status");
    let mut lines = Vec::new();

    let rank_stats = status
        .and_then(|s| s.get("rankStats"))
        .and_then(|v| v.as_array());

    if let Some(ranks) = rank_stats {
        let rows: Vec<Vec<Cell2>> = ranks
            .iter()
            .map(|rank| {
                let rank_id = rank.get("rank").and_then(|v| v.as_i64()).unwrap_or(0);
                let avg_latency = rank
                    .get("avgLatencyNs")
                    .and_then(|v| v.as_i64())
                    .unwrap_or(0);
                let total_bytes = rank.get("totalBytes").and_then(|v| v.as_i64()).unwrap_or(0);
                let is_straggler = rank
                    .get("isStraggler")
                    .and_then(|v| v.as_bool())
                    .unwrap_or(false);
                let straggler = if is_straggler {
                    ("YES".to_string(), Some(Marker::Error))
                } else {
                    ("no".to_string(), Some(Marker::Disabled))
                };
                vec![
                    (rank_id.to_string(), None),
                    (format_ns(avg_latency), None),
                    (format_bytes(total_bytes), None),
                    straggler,
                ]
            })
            .collect();
        lines.extend(ui::grid(
            &["RANK", "AVG LATENCY", "TOTAL BYTES", "STRAGGLER"],
            &rows,
            color,
        ));
    } else {
        lines.push(hint("No rank statistics available.", color));
    }

    let comm_pattern = status
        .and_then(|s| s.get("commPattern"))
        .and_then(|v| v.as_str())
        .unwrap_or("-");
    let ratio = status
        .and_then(|s| s.get("commComputeRatio"))
        .and_then(|v| v.as_f64())
        .unwrap_or(0.0);
    let bottleneck = status
        .and_then(|s| s.get("bottleneck"))
        .and_then(|v| v.as_str())
        .unwrap_or("-");

    lines.push(String::new());
    lines.push(ui::section("Communication Analysis", color));
    lines.push(ui::kv("Pattern", comm_pattern, 14, color));
    lines.push(ui::kv("Comm/Compute", &format!("{ratio:.2}"), 14, color));
    lines.push(ui::kv(
        "Bottleneck",
        &bottleneck_marker(bottleneck).paint_with(bottleneck, color),
        14,
        color,
    ));
    lines
}

/// Host/device transfer counters the gateway sums over every collector (`/api/gpu/memory`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct MemoryStats {
    pub h2d_bytes: u64,
    pub d2h_bytes: u64,
    pub d2d_bytes: u64,
    pub h2d_count: u64,
    pub d2h_count: u64,
    pub d2d_count: u64,
    /// (reachable, total) collectors behind the sums, when the gateway says.
    pub collectors: Option<(u64, u64)>,
}

/// Parse the gateway's `/api/gpu/memory` body.
pub fn parse_memory(body: &serde_json::Value) -> MemoryStats {
    let n = |k: &str| body.get(k).and_then(|v| v.as_u64()).unwrap_or(0);
    let collectors = body.get("collectors").map(|c| {
        (
            c.get("reachable").and_then(|v| v.as_u64()).unwrap_or(0),
            c.get("total").and_then(|v| v.as_u64()).unwrap_or(0),
        )
    });
    MemoryStats {
        h2d_bytes: n("h2dBytes"),
        d2h_bytes: n("d2hBytes"),
        d2d_bytes: n("d2dBytes"),
        h2d_count: n("h2dCount"),
        d2h_count: n("d2hCount"),
        d2d_count: n("d2dCount"),
        collectors,
    }
}

/// `gpu memory` through the gateway. The counters are summed over all collectors and cumulative since each
/// collector started; the gateway has no per-node view, so `--node` is not applied.
pub async fn execute_memory_gateway(gw: &GatewayClient, node: Option<&str>) -> Result<()> {
    let color = ui::color_enabled();
    println!("{}", ui::header("GPU Memory Transfer Stats", color));
    println!();
    let body = gw
        .get_json("/api/gpu/memory")
        .await
        .context("could not read GPU memory counters from the gateway")?;
    let stats = parse_memory(&body);
    if let Some(node) = node {
        println!(
            "{}",
            hint(
                &format!(
                    "--node {node} is not applied: the gateway reports totals over all collectors."
                ),
                color
            )
        );
    }
    print_lines(memory_lines(&stats, color));
    println!();
    Ok(())
}

/// Without a gateway there is no source: the counters live in the collectors, not in Kubernetes objects.
pub fn execute_memory_without_gateway(node: Option<&str>) -> Result<()> {
    let color = ui::color_enabled();
    let title = match node {
        Some(n) => format!("GPU Memory Transfer Stats: {n}"),
        None => "GPU Memory Transfer Stats".to_string(),
    };
    println!("{}", ui::header(&title, color));
    println!();
    println!(
        "{}",
        hint(
            "GPU memory transfer counters are read from the eBPF collectors through the API gateway.",
            color
        )
    );
    println!(
        "{}",
        hint(
            "Set GRYVIA_GATEWAY_URL (or --gateway) and GRYVIA_API_KEY; the collector must be deployed (helm ebpf.enabled).",
            color
        )
    );
    println!();
    Ok(())
}

/// Human-readable byte count (binary units).
fn human_bytes(n: u64) -> String {
    const UNITS: [&str; 6] = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
    let mut v = n as f64;
    let mut i = 0;
    while v >= 1024.0 && i < UNITS.len() - 1 {
        v /= 1024.0;
        i += 1;
    }
    if i == 0 {
        format!("{n} B")
    } else {
        format!("{v:.1} {}", UNITS[i])
    }
}

/// Lines of the memory transfer table (cumulative counters; there is no bandwidth or latency source).
pub fn memory_lines(stats: &MemoryStats, color: bool) -> Vec<String> {
    let row = |name: &str, bytes: u64, count: u64| -> Vec<Cell2> {
        vec![
            (name.to_string(), None),
            (human_bytes(bytes), None),
            (count.to_string(), None),
            (
                bytes
                    .checked_div(count)
                    .map(human_bytes)
                    .unwrap_or_else(|| "-".to_string()),
                None,
            ),
        ]
    };
    let rows = vec![
        row("H2D (Host->Device)", stats.h2d_bytes, stats.h2d_count),
        row("D2H (Device->Host)", stats.d2h_bytes, stats.d2h_count),
        row("D2D (Device->Device)", stats.d2d_bytes, stats.d2d_count),
    ];
    let mut lines = ui::grid(
        &["DIRECTION", "BYTES", "TRANSFERS", "AVG SIZE"],
        &rows,
        color,
    );
    if let Some((reachable, total)) = stats.collectors {
        let text = if total == 0 {
            "no collector found; the counters are zero because nothing measures them".to_string()
        } else {
            format!("{reachable} of {total} collectors answered")
        };
        lines.push(String::new());
        lines.push(format!(
            "{} {}",
            if reachable < total || total == 0 {
                Marker::Warn.paint_with(Marker::Warn.glyph(), color)
            } else {
                Marker::Ok.paint_with(Marker::Ok.glyph(), color)
            },
            text
        ));
    }
    lines
}

/// `gpu rdma`: the fabric signals of jobs (`GryviaFabricSignal.status`, published by the collector with
/// `ebpf.publishFabricStatus`) and, with `--node`, that node's `GryviaNodeFabric`. There are no per-NIC
/// figures in these objects; a rate that is absent was not measured (never shown as zero).
async fn execute_rdma(
    client: &GryviaClient,
    node: Option<&str>,
    job: Option<&str>,
    namespace: &str,
) -> Result<()> {
    let color = ui::color_enabled();
    let title = match (node, job) {
        (_, Some(j)) => format!("RDMA / Fabric Signals: job {j}"),
        (Some(n), None) => format!("RDMA / Fabric Signals: node {n}"),
        _ => "RDMA / Fabric Signals".to_string(),
    };
    println!("{}", ui::header(&title, color));
    println!();

    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaFabricSignal",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);
    match api.list(&ListParams::default()).await {
        Ok(list) => {
            let signals: Vec<&DynamicObject> = list
                .items
                .iter()
                .filter(|o| {
                    job.is_none_or(|j| {
                        o.data
                            .get("spec")
                            .and_then(|s| s.get("jobRef"))
                            .and_then(|v| v.as_str())
                            == Some(j)
                    })
                })
                .collect();
            if signals.is_empty() {
                println!(
                    "{}",
                    hint(
                        &format!("No GryviaFabricSignal found in namespace {namespace}."),
                        color
                    )
                );
                println!(
                    "{}",
                    hint(
                        "Create one per job (spec.jobRef) and run the collector with ebpf.publishFabricStatus and ebpf.nicCounters.",
                        color
                    )
                );
            } else {
                print_lines(rdma_lines(&signals, color));
            }
        }
        Err(e) => {
            display::print_warning(&format!(
                "Could not query fabric signals: {}",
                display::cluster_error_text(&e.to_string())
            ));
        }
    }

    if let Some(node) = node {
        println!();
        let nf_ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
            "gryvia.io",
            "v1alpha1",
            "GryviaNodeFabric",
        ));
        let nf_api: Api<DynamicObject> = Api::all_with(client.kube_client.clone(), &nf_ar);
        match nf_api.get(node).await {
            Ok(nf) => print_lines(node_fabric_lines(&nf, color)),
            Err(_) => println!(
                "{}",
                hint(
                    &format!("No GryviaNodeFabric for node {node} (needs ebpf.publishNodeFabric)."),
                    color
                )
            ),
        }
    }
    println!();
    Ok(())
}

fn rate(status: Option<&serde_json::Value>, key: &str, unit: &str) -> String {
    match status.and_then(|s| s.get(key)).and_then(|v| v.as_f64()) {
        Some(v) => format!("{}{unit}", trim_float(v)),
        None => "-".to_string(),
    }
}

fn trim_float(v: f64) -> String {
    let s = format!("{v:.4}");
    s.trim_end_matches('0').trim_end_matches('.').to_string()
}

/// Lines of the per-job fabric signal table.
pub fn rdma_lines(signals: &[&DynamicObject], color: bool) -> Vec<String> {
    let rows: Vec<Vec<Cell2>> = signals
        .iter()
        .map(|o| {
            let status = o.data.get("status");
            let job = o
                .data
                .get("spec")
                .and_then(|s| s.get("jobRef"))
                .and_then(|v| v.as_str())
                .unwrap_or("-");
            let updated = status
                .and_then(|s| s.get("updatedAt"))
                .and_then(|v| v.as_str())
                .unwrap_or("-");
            vec![
                (job.to_string(), None),
                (rate(status, "rdmaRetryRate", ""), None),
                (rate(status, "cnpRate", "/s"), None),
                (rate(status, "pfcRate", "/s"), None),
                (rate(status, "ncclP99ms", " ms"), None),
                (rate(status, "scoreDelta", ""), None),
                (updated.to_string(), None),
            ]
        })
        .collect();
    ui::grid(
        &[
            "JOB",
            "RETRY RATIO",
            "CNP",
            "PFC",
            "NCCL P99",
            "SCORE DELTA",
            "UPDATED",
        ],
        &rows,
        color,
    )
}

/// Lines of a node's fabric health.
pub fn node_fabric_lines(nf: &DynamicObject, color: bool) -> Vec<String> {
    let spec = nf.data.get("spec");
    let mut lines = vec![ui::section("Node Fabric", color)];
    lines.push(ui::kv(
        "Score delta",
        &rate(spec, "scoreDelta", ""),
        13,
        color,
    ));
    let reasons: Vec<&str> = spec
        .and_then(|s| s.get("reasons"))
        .and_then(|v| v.as_array())
        .map(|a| a.iter().filter_map(|r| r.as_str()).collect())
        .unwrap_or_default();
    lines.push(ui::kv(
        "Reasons",
        &if reasons.is_empty() {
            "-".to_string()
        } else {
            reasons.join(", ")
        },
        13,
        color,
    ));
    lines.push(ui::kv(
        "Measured at",
        spec.and_then(|s| s.get("measuredAt"))
            .and_then(|v| v.as_str())
            .unwrap_or("-"),
        13,
        color,
    ));
    lines
}

async fn execute_training(client: &GryviaClient, job: &str, namespace: &str) -> Result<()> {
    let color = ui::color_enabled();
    println!(
        "{}",
        ui::header(&format!("Training Insights: {job}"), color)
    );
    println!();

    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaTrainingInsight",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    // Try to get insight by job name
    let insight = match api.get(job).await {
        Ok(i) => i,
        Err(_) => {
            // Try listing with label
            let label_selector = format!("gryvia.io/job={}", job);
            let params = ListParams::default().labels(&label_selector);
            match api.list(&params).await {
                Ok(list) if !list.items.is_empty() => list.items[0].clone(),
                _ => {
                    println!(
                        "{}",
                        hint("No training insights found for this job.", color)
                    );
                    println!();
                    return Ok(());
                }
            }
        }
    };

    print_lines(training_lines(&insight, color));
    println!();
    Ok(())
}

/// Lines of the straggler detection and communication analysis sections.
pub fn training_lines(insight: &DynamicObject, color: bool) -> Vec<String> {
    let status = insight.data.get("status");
    let mut lines = Vec::new();

    let stragglers = status
        .and_then(|s| s.get("stragglers"))
        .and_then(|v| v.as_array());

    lines.push(ui::section("Straggler Detection", color));
    match stragglers {
        Some(list) if list.is_empty() => {
            lines.push(format!(
                "{} No stragglers detected.",
                Marker::Ok.paint_with(Marker::Ok.glyph(), color)
            ));
        }
        Some(list) => {
            for straggler in list {
                let rank = straggler.get("rank").and_then(|v| v.as_i64()).unwrap_or(0);
                let factor = straggler
                    .get("slowdownFactor")
                    .and_then(|v| v.as_f64())
                    .unwrap_or(0.0);
                let reason = straggler
                    .get("reason")
                    .and_then(|v| v.as_str())
                    .unwrap_or("-");
                let what = if factor > 0.0 {
                    format!("is {factor:.1}x slower")
                } else {
                    "is flagged as a straggler".to_string()
                };
                lines.push(format!(
                    "{} Rank {} {} ({})",
                    Marker::Error.paint_with(Marker::Error.glyph(), color),
                    rank,
                    what,
                    reason
                ));
            }
        }
        None => lines.push(hint("No straggler data available.", color)),
    }
    lines.push(String::new());

    let comm_pattern = status
        .and_then(|s| s.get("commPattern"))
        .and_then(|v| v.as_str())
        .unwrap_or("-");
    let ratio = status
        .and_then(|s| s.get("commComputeRatio"))
        .and_then(|v| v.as_f64())
        .unwrap_or(0.0);
    let bottleneck = status
        .and_then(|s| s.get("bottleneck"))
        .and_then(|v| v.as_str())
        .unwrap_or("-");
    let last_analysis = status
        .and_then(|s| s.get("lastAnalysis"))
        .and_then(|v| v.as_str())
        .unwrap_or("-");

    let explanation = match bottleneck {
        "communication" => " - Network is the primary bottleneck",
        "compute" => " - GPU compute is the primary bottleneck",
        "data_loading" => " - Data loading is the primary bottleneck",
        _ => "",
    };
    let bottleneck_text = format!(
        "{}{}",
        bottleneck_marker(bottleneck).paint_with(bottleneck, color),
        explanation
    );

    lines.push(ui::section("Communication Analysis", color));
    lines.push(ui::kv("Pattern", comm_pattern, 15, color));
    lines.push(ui::kv("Comm/Compute", &format!("{ratio:.2}"), 15, color));
    lines.push(ui::kv("Bottleneck", &bottleneck_text, 15, color));
    lines.push(ui::kv("Last Analysis", last_analysis, 15, color));
    lines
}

fn format_ns(ns: i64) -> String {
    if ns >= 1_000_000_000 {
        format!("{:.2}s", ns as f64 / 1_000_000_000.0)
    } else if ns >= 1_000_000 {
        format!("{:.2}ms", ns as f64 / 1_000_000.0)
    } else if ns >= 1_000 {
        format!("{:.2}us", ns as f64 / 1_000.0)
    } else {
        format!("{}ns", ns)
    }
}

fn format_bytes(bytes: i64) -> String {
    if bytes >= 1_073_741_824 {
        format!("{:.2} GB", bytes as f64 / 1_073_741_824.0)
    } else if bytes >= 1_048_576 {
        format!("{:.2} MB", bytes as f64 / 1_048_576.0)
    } else if bytes >= 1024 {
        format!("{:.2} KB", bytes as f64 / 1024.0)
    } else {
        format!("{} B", bytes)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::commands::network::strip_ansi;
    use serde_json::json;

    fn insight(status: serde_json::Value) -> DynamicObject {
        serde_json::from_value(json!({
            "apiVersion": "gryvia.io/v1alpha1",
            "kind": "GryviaTrainingInsight",
            "metadata": {"name": "job"},
            "status": status,
        }))
        .unwrap()
    }

    fn nccl() -> DynamicObject {
        insight(json!({
            "rankStats": [
                {"rank": 0, "avgLatencyNs": 1_500_000, "totalBytes": 2_147_483_648i64, "isStraggler": false},
                {"rank": 1, "avgLatencyNs": 9_000_000, "totalBytes": 1024, "isStraggler": true},
            ],
            "commPattern": "ring",
            "commComputeRatio": 0.4567,
            "bottleneck": "communication",
        }))
    }

    #[test]
    fn nccl_renders() {
        assert_eq!(
            nccl_lines(&nccl(), false),
            vec![
                "RANK  AVG LATENCY  TOTAL BYTES  STRAGGLER",
                "0     1.50ms       2.00 GB      no",
                "1     9.00ms       1.00 KB      YES",
                "",
                "Communication Analysis",
                "Pattern       ring",
                "Comm/Compute  0.46",
                "Bottleneck    communication",
            ]
        );
    }

    #[test]
    fn nccl_without_ranks_hints() {
        let lines = nccl_lines(&insight(json!({})), false);
        assert_eq!(lines[0], "! No rank statistics available.");
    }

    #[test]
    fn memory_table_renders() {
        let stats = parse_memory(
            &json!({"h2dBytes": 3221225472u64, "h2dCount": 3, "d2hBytes": 1024,
            "d2hCount": 2, "d2dBytes": 0, "d2dCount": 0, "collectors": {"reachable": 2, "total": 3}}),
        );
        assert_eq!(
            memory_lines(&stats, false),
            vec![
                "DIRECTION             BYTES    TRANSFERS  AVG SIZE",
                "H2D (Host->Device)    3.0 GiB  3          1.0 GiB",
                "D2H (Device->Host)    1.0 KiB  2          512 B",
                "D2D (Device->Device)  0 B      0          -",
                "",
                "! 2 of 3 collectors answered",
            ]
        );
        let none = parse_memory(&json!({"collectors": {"reachable": 0, "total": 0}}));
        assert!(memory_lines(&none, false)
            .last()
            .unwrap()
            .contains("no collector found"));
    }

    #[tokio::test]
    async fn gateway_memory_is_read() {
        use crate::gateway::{mock, GatewayClient, GatewayConfig};
        let body = json!({"h2dBytes": 10, "h2dCount": 1, "d2hBytes": 0, "d2hCount": 0,
            "d2dBytes": 0, "d2dCount": 0, "collectors": {"reachable": 1, "total": 1}});
        let gw = mock::start(vec![("/api/gpu/memory", 200, body.to_string())]).await;
        let client = GatewayClient::new(GatewayConfig {
            base_url: gw.url.clone(),
            api_key: Some("k".into()),
            insecure: false,
            ca_file: None,
            timeout: std::time::Duration::from_secs(5),
        });
        let v = client.get_json("/api/gpu/memory").await.unwrap();
        assert_eq!(parse_memory(&v).h2d_bytes, 10);
        assert_eq!(parse_memory(&v).collectors, Some((1, 1)));
    }

    #[test]
    fn rdma_table_shows_unmeasured_as_dash() {
        let sig = |job: &str, status: serde_json::Value| -> DynamicObject {
            serde_json::from_value(json!({
                "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaFabricSignal",
                "metadata": {"name": job}, "spec": {"jobRef": job}, "status": status
            }))
            .unwrap()
        };
        let a = sig(
            "llama",
            json!({"rdmaRetryRate": 0.0021, "cnpRate": 12.5, "pfcRate": 3.0, "ncclP99ms": 42.5,
            "scoreDelta": 0.4, "updatedAt": "2026-01-01T00:00:00Z"}),
        );
        let b = sig("bert", json!({}));
        assert_eq!(
            rdma_lines(&[&a, &b], false),
            vec![
                "JOB    RETRY RATIO  CNP     PFC  NCCL P99  SCORE DELTA  UPDATED",
                "llama  0.0021       12.5/s  3/s  42.5 ms   0.4          2026-01-01T00:00:00Z",
                "bert   -            -       -    -         -            -",
            ]
        );
    }

    #[test]
    fn training_renders() {
        let i = insight(json!({
            "stragglers": [{"rank": 3, "slowdownFactor": 2.25, "reason": "thermal throttling"}],
            "commPattern": "tree",
            "commComputeRatio": 1.0,
            "bottleneck": "compute",
            "lastAnalysis": "2026-09-29T10:00:00Z",
        }));
        assert_eq!(
            training_lines(&i, false),
            vec![
                "Straggler Detection",
                "✗ Rank 3 is 2.2x slower (thermal throttling)",
                "",
                "Communication Analysis",
                "Pattern        tree",
                "Comm/Compute   1.00",
                "Bottleneck     compute - GPU compute is the primary bottleneck",
                "Last Analysis  2026-09-29T10:00:00Z",
            ]
        );
    }

    #[test]
    fn color_strips_to_plain() {
        for f in [
            nccl_lines as fn(&DynamicObject, bool) -> Vec<String>,
            training_lines,
        ] {
            let plain = f(&nccl(), false);
            let colored = f(&nccl(), true);
            assert!(colored.iter().any(|l| l.contains('\x1b')));
            let stripped: Vec<String> = colored.iter().map(|l| strip_ansi(l)).collect();
            assert_eq!(stripped, plain);
        }
    }
}
