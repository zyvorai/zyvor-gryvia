// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

use anyhow::{Context, Result};
use kube::api::{Api, ApiResource, GroupVersionKind, ListParams};
use kube::core::DynamicObject;
use std::collections::{HashMap, HashSet};

use crate::client::GryviaClient;
use crate::commands::flows::verdict_marker;
use crate::display;
use crate::gateway::GatewayClient;
use crate::ui::{self, Marker};

/// `network graph` through the gateway: `/api/network/graph` (edges merged from every GryviaServiceGraph).
/// The gateway returns every namespace; `--graph-namespace` does not apply.
pub async fn execute_gateway(gw: &GatewayClient, format: &str) -> Result<()> {
    let (services, edges) = fetch_gateway(gw).await?;
    show(&services, &edges, format)
}

/// The services and edges the gateway reports.
pub async fn fetch_gateway(gw: &GatewayClient) -> Result<(HashSet<String>, Vec<Edge>)> {
    let body = gw
        .get_json("/api/network/graph")
        .await
        .context("could not read the service graph from the gateway")?;
    Ok(parse_gateway_graph(&body))
}

/// Parse the gateway graph (`{"nodes": [{"id"}], "edges": [{"source","target","protocol","latency","verdict"}]}`).
pub fn parse_gateway_graph(body: &serde_json::Value) -> (HashSet<String>, Vec<Edge>) {
    let text = |v: &serde_json::Value, k: &str, d: &str| {
        v.get(k)
            .and_then(|x| x.as_str())
            .filter(|x| !x.is_empty())
            .unwrap_or(d)
            .to_string()
    };
    let mut services = HashSet::new();
    for n in body
        .get("nodes")
        .and_then(|v| v.as_array())
        .into_iter()
        .flatten()
    {
        services.insert(text(n, "id", "unknown"));
    }
    let mut edges = Vec::new();
    for e in body
        .get("edges")
        .and_then(|v| v.as_array())
        .into_iter()
        .flatten()
    {
        let edge = Edge {
            source: text(e, "source", "unknown"),
            destination: text(e, "target", "unknown"),
            latency: text(e, "latency", "-"),
            protocol: text(e, "protocol", "TCP").to_ascii_uppercase(),
            verdict: text(e, "verdict", ""),
        };
        services.insert(edge.source.clone());
        services.insert(edge.destination.clone());
        edges.push(edge);
    }
    (services, edges)
}

/// `network graph` without a gateway: the status of the GryviaServiceGraph objects in the namespace.
pub async fn execute(client: &GryviaClient, namespace: &str, format: &str) -> Result<()> {
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaServiceGraph",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let graphs = match api.list(&ListParams::default()).await {
        Ok(list) => list,
        Err(e) => {
            display::print_warning(&format!(
                "Could not query service graphs: {}",
                display::cluster_error_text(&e.to_string())
            ));
            println!("  Showing empty graph.");
            println!();
            return Ok(());
        }
    };
    let (services, edges) = graphs_to_graph(&graphs.items);
    show(&services, &edges, format)
}

/// Services and edges of GryviaServiceGraph objects (`status.nodes`, `status.edges`).
pub fn graphs_to_graph(graphs: &[DynamicObject]) -> (HashSet<String>, Vec<Edge>) {
    let mut services = HashSet::new();
    let mut edges = Vec::new();
    let mut seen = HashSet::new();
    for g in graphs {
        let status = g.data.get("status");
        for n in status
            .and_then(|s| s.get("nodes"))
            .and_then(|v| v.as_array())
            .into_iter()
            .flatten()
        {
            if let Some(name) = n.get("name").and_then(|v| v.as_str()) {
                services.insert(name.to_string());
            }
        }
        for e in status
            .and_then(|s| s.get("edges"))
            .and_then(|v| v.as_array())
            .into_iter()
            .flatten()
        {
            let text = |k: &str| e.get(k).and_then(|v| v.as_str()).unwrap_or("").to_string();
            let (source, destination) = (text("source"), text("destination"));
            if source.is_empty() || destination.is_empty() {
                continue;
            }
            let port = e.get("port").and_then(|v| v.as_u64()).unwrap_or(0);
            let protocol = text("protocol");
            if !seen.insert((source.clone(), destination.clone(), protocol.clone(), port)) {
                continue;
            }
            let latency = ["latencyP99", "latencyP50"]
                .iter()
                .map(|k| text(k))
                .find(|s| !s.is_empty())
                .unwrap_or_else(|| "-".to_string());
            services.insert(source.clone());
            services.insert(destination.clone());
            edges.push(Edge {
                source,
                destination,
                latency,
                protocol: if protocol.is_empty() {
                    "TCP".to_string()
                } else {
                    protocol.to_ascii_uppercase()
                },
                verdict: text("verdict").to_ascii_uppercase(),
            });
        }
    }
    (services, edges)
}

fn show(services: &HashSet<String>, edges: &[Edge], format: &str) -> Result<()> {
    match format {
        "json" => print_json_graph(services, edges)?,
        _ => {
            for line in ascii_graph_lines(services, edges, ui::color_enabled()) {
                println!("{line}");
            }
        }
    }
    Ok(())
}

pub struct Edge {
    pub source: String,
    pub destination: String,
    pub latency: String,
    pub protocol: String,
    /// Empty when the source does not report verdicts.
    pub verdict: String,
}

/// Lines of the ascii service dependency graph.
fn ascii_graph_lines(services: &HashSet<String>, edges: &[Edge], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("Service Dependency Graph", color), String::new()];

    if services.is_empty() {
        lines.push(Marker::Disabled.paint_with("No services or flows found.", color));
        lines.push(String::new());
        return lines;
    }

    // Group edges by source
    let mut adjacency: HashMap<&str, Vec<&Edge>> = HashMap::new();
    for edge in edges {
        adjacency.entry(&edge.source).or_default().push(edge);
    }

    let mut sorted_sources: Vec<&str> = edges
        .iter()
        .map(|e| e.source.as_str())
        .collect::<HashSet<&str>>()
        .into_iter()
        .collect();
    sorted_sources.sort();

    let mut printed: HashSet<&str> = HashSet::new();
    for source in &sorted_sources {
        if printed.contains(source) {
            continue;
        }
        node_lines(source, &adjacency, &mut printed, color, &mut lines);
    }

    // Services that only appear as destinations (leaf nodes)
    let mut remaining: Vec<&String> = services
        .iter()
        .filter(|s| !printed.contains(s.as_str()))
        .collect();
    remaining.sort();

    for svc in remaining {
        lines.push(format!("  {}", ui::ansi(&format!("[{svc}]"), "1", color)));
        lines.push(format!(
            "    {}",
            Marker::Disabled.paint_with("(leaf) (no outgoing connections)", color)
        ));
        lines.push(String::new());
    }

    lines.push(format!(
        "{} {} services, {} connections",
        ui::ansi("ℹ", "1;36", color),
        services.len(),
        edges.len()
    ));
    lines.push(String::new());
    lines
}

fn node_lines<'a>(
    service: &'a str,
    adjacency: &HashMap<&'a str, Vec<&Edge>>,
    printed: &mut HashSet<&'a str>,
    color: bool,
    lines: &mut Vec<String>,
) {
    if printed.contains(service) {
        lines.push(format!(
            "  {} (circular ref)",
            Marker::Disabled.paint_with(&format!("[{service}]"), color)
        ));
        return;
    }
    printed.insert(service);
    lines.push(format!(
        "  {}",
        ui::ansi(&format!("[{service}]"), "1", color)
    ));

    if let Some(targets) = adjacency.get(service) {
        for (i, edge) in targets.iter().enumerate() {
            let connector = if i == targets.len() - 1 {
                "└──"
            } else {
                "├──"
            };
            let (marker, symbol) = if edge.verdict.is_empty() {
                (Marker::Disabled, "→")
            } else {
                let marker = verdict_marker(&edge.verdict);
                let symbol = match marker {
                    Marker::Ok => "→",
                    Marker::Error => "✗",
                    _ => "?",
                };
                (marker, symbol)
            };
            let latency = if edge.latency == "-" {
                String::new()
            } else {
                format!(
                    " {}",
                    Marker::Disabled.paint_with(&format!("({})", edge.latency), color)
                )
            };
            lines.push(format!(
                "    {} {} {} {}{}",
                Marker::Disabled.paint_with(connector, color),
                marker.paint_with(symbol, color),
                edge.destination,
                edge.protocol,
                latency,
            ));
        }
    }
    lines.push(String::new());
}

fn print_json_graph(services: &HashSet<String>, edges: &[Edge]) -> Result<()> {
    let nodes: Vec<serde_json::Value> = services
        .iter()
        .map(|s| {
            serde_json::json!({
                "id": s,
                "label": s,
            })
        })
        .collect();

    let edge_values: Vec<serde_json::Value> = edges
        .iter()
        .map(|e| {
            serde_json::json!({
                "source": e.source,
                "target": e.destination,
                "protocol": e.protocol,
                "latency": e.latency,
                "verdict": e.verdict,
            })
        })
        .collect();

    let graph = serde_json::json!({
        "nodes": nodes,
        "edges": edge_values,
    });

    println!("{}", serde_json::to_string_pretty(&graph)?);

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::commands::network::strip_ansi;

    fn edge(src: &str, dst: &str, latency: &str, verdict: &str) -> Edge {
        Edge {
            source: src.into(),
            destination: dst.into(),
            latency: latency.into(),
            protocol: "TCP".into(),
            verdict: verdict.into(),
        }
    }

    fn fixture() -> (HashSet<String>, Vec<Edge>) {
        let edges = vec![
            edge("web", "api", "3ms", "FORWARDED"),
            edge("web", "db", "-", "DROP"),
            edge("api", "db", "1ms", "ALLOW"),
        ];
        let services = ["web", "api", "db"].iter().map(|s| s.to_string()).collect();
        (services, edges)
    }

    #[test]
    fn ascii_graph_renders() {
        let (services, edges) = fixture();
        assert_eq!(
            ascii_graph_lines(&services, &edges, false),
            vec![
                "━━━ Service Dependency Graph ━━━",
                "",
                "  [api]",
                "    └── → db TCP (1ms)",
                "",
                "  [web]",
                "    ├── → api TCP (3ms)",
                "    └── ✗ db TCP",
                "",
                "  [db]",
                "    (leaf) (no outgoing connections)",
                "",
                "ℹ 3 services, 3 connections",
                "",
            ]
        );
    }

    #[test]
    fn ascii_graph_color_strips_to_plain() {
        let (services, edges) = fixture();
        let plain = ascii_graph_lines(&services, &edges, false);
        let colored = ascii_graph_lines(&services, &edges, true);
        assert!(colored.iter().any(|l| l.contains('\x1b')));
        let stripped: Vec<String> = colored.iter().map(|l| strip_ansi(l)).collect();
        assert_eq!(stripped, plain);
    }

    #[tokio::test]
    async fn gateway_graph_renders() {
        use crate::gateway::{mock, GatewayConfig};
        let body = serde_json::json!({
            "nodes": [{"id": "web", "label": "web", "health": "unknown"}, {"id": "api"}],
            "edges": [{"source": "web", "target": "api", "protocol": "tcp", "latency": "4.3ms", "verdict": ""}]
        });
        let gw = mock::start(vec![("/api/network/graph", 200, body.to_string())]).await;
        let client = GatewayClient::new(GatewayConfig {
            base_url: gw.url.clone(),
            api_key: None,
            insecure: false,
            ca_file: None,
            timeout: std::time::Duration::from_secs(5),
        });
        let (services, edges) = fetch_gateway(&client).await.unwrap();
        assert_eq!(
            ascii_graph_lines(&services, &edges, false),
            vec![
                "━━━ Service Dependency Graph ━━━",
                "",
                "  [web]",
                "    └── → api TCP (4.3ms)",
                "",
                "  [api]",
                "    (leaf) (no outgoing connections)",
                "",
                "ℹ 2 services, 1 connections",
                "",
            ]
        );
    }

    #[test]
    fn service_graph_status_becomes_a_graph() {
        let g: DynamicObject = serde_json::from_value(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaServiceGraph", "metadata": {"name": "g"},
            "status": {"nodes": [{"name": "db"}], "edges": [
                {"source": "web", "destination": "api", "protocol": "tcp", "port": 80, "latencyP50": "2.0ms"},
                {"source": "web", "destination": "api", "protocol": "tcp", "port": 80}]}
        }))
        .unwrap();
        let (services, edges) = graphs_to_graph(&[g]);
        assert_eq!(services.len(), 3);
        assert_eq!(edges.len(), 1, "the same edge in two graphs is one edge");
        assert_eq!(edges[0].latency, "2.0ms");
        assert_eq!(edges[0].verdict, "");
    }

    #[test]
    fn empty_graph_says_so() {
        let lines = ascii_graph_lines(&HashSet::new(), &[], false);
        assert_eq!(lines[2], "No services or flows found.");
    }
}
