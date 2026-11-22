// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use kube::api::{Api, ApiResource, GroupVersionKind, ListParams};
use kube::core::DynamicObject;

use crate::client::GryviaClient;
use crate::display;
use crate::gateway::GatewayClient;
use crate::ui::{self, Cell2, Marker};

/// Where the flows shown by `network flows` came from.
const SRC_GATEWAY_NETRA: &str = "gateway (Netra flow history)";
const SRC_GATEWAY_GRAPH: &str = "gateway (service graph edges)";
const SRC_KUBE_GRAPH: &str = "GryviaServiceGraph edges (kubernetes)";

/// `network flows` through the gateway: `/api/network/flows` (Netra history when the gateway has Netra,
/// otherwise the service-graph edges). The gateway returns every namespace and has a fixed window, so
/// `--flow-namespace` and `--last` do not apply here; `--service` filters client-side.
pub async fn execute_gateway(
    gw: &GatewayClient,
    service: Option<&str>,
    output: &str,
) -> Result<()> {
    let (items, source) = fetch_gateway(gw).await?;
    render(items, service, None, None, source, output)
}

/// The flow items the gateway returns and the label of their source.
pub async fn fetch_gateway(gw: &GatewayClient) -> Result<(Vec<serde_json::Value>, &'static str)> {
    let body = gw
        .get_json("/api/network/flows")
        .await
        .context("could not read network flows from the gateway")?;
    let source = match body.get("source").and_then(|v| v.as_str()) {
        Some("netra") => SRC_GATEWAY_NETRA,
        _ => SRC_GATEWAY_GRAPH,
    };
    let items = body
        .get("items")
        .and_then(|v| v.as_array())
        .cloned()
        .unwrap_or_default();
    Ok((items, source))
}

/// `network flows` without a gateway: the edges of the GryviaServiceGraph objects in the namespace
/// (there is no per-flow Kubernetes kind).
pub async fn execute(
    client: &GryviaClient,
    service: Option<&str>,
    namespace: &str,
    last: &str,
    output: &str,
) -> Result<()> {
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaServiceGraph",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);
    let graphs = match api.list(&ListParams::default()).await {
        Ok(list) => list,
        Err(e) => {
            let color = ui::color_enabled();
            for line in
                flows_header_lines(service, Some(namespace), Some(last), SRC_KUBE_GRAPH, color)
            {
                println!("{line}");
            }
            display::print_warning(&format!(
                "Could not query service graphs: {}",
                display::cluster_error_text(&e.to_string())
            ));
            println!();
            return Ok(());
        }
    };
    let items = graph_edge_items(&graphs.items);
    render(
        items,
        service,
        Some(namespace),
        Some(last),
        SRC_KUBE_GRAPH,
        output,
    )
}

/// The edges of GryviaServiceGraph objects as flow items in the shape the gateway returns
/// (`{"metadata": .., "spec": {timestamp, source, destination, protocol, port, bytes, latency, verdict}}`).
pub fn graph_edge_items(graphs: &[DynamicObject]) -> Vec<serde_json::Value> {
    let mut items = Vec::new();
    for g in graphs {
        let name = g.metadata.name.as_deref().unwrap_or("graph");
        let status = g.data.get("status");
        let updated = status
            .and_then(|s| s.get("lastUpdated"))
            .and_then(|v| v.as_str())
            .unwrap_or("");
        let edges = status
            .and_then(|s| s.get("edges"))
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();
        for (i, e) in edges.iter().enumerate() {
            let text = |k: &str| e.get(k).and_then(|v| v.as_str()).unwrap_or("").to_string();
            let latency = ["latencyP99", "latencyP50"]
                .iter()
                .map(|k| text(k))
                .find(|s| !s.is_empty())
                .unwrap_or_default();
            items.push(serde_json::json!({
                "metadata": {"name": format!("{name}-{i}")},
                "spec": {
                    "timestamp": updated,
                    "source": text("source"),
                    "destination": text("destination"),
                    "protocol": text("protocol").to_ascii_uppercase(),
                    "port": e.get("port").cloned().unwrap_or(serde_json::json!(0)),
                    "bytes": text("throughput"),
                    "latency": latency,
                    "verdict": text("verdict").to_ascii_uppercase(),
                }
            }));
        }
    }
    items
}

fn render(
    mut items: Vec<serde_json::Value>,
    service: Option<&str>,
    namespace: Option<&str>,
    last: Option<&str>,
    source: &str,
    output: &str,
) -> Result<()> {
    let color = ui::color_enabled();
    if let Some(svc) = service {
        items.retain(|i| {
            let spec = i.get("spec");
            ["source", "destination"].iter().any(|k| {
                spec.and_then(|s| s.get(*k))
                    .and_then(|v| v.as_str())
                    .is_some_and(|v| v.contains(svc))
            })
        });
    }
    if matches!(output, "json" | "yaml") {
        return crate::output::print_serialized(output, &items);
    }
    for line in flows_header_lines(service, namespace, last, source, color) {
        println!("{line}");
    }
    if items.is_empty() {
        println!(
            "{}",
            Marker::Disabled.paint_with("No network flows found.", color)
        );
        println!();
        return Ok(());
    }
    print_flows_table(&items);
    Ok(())
}

/// Marker for a flow verdict (`FORWARDED`, `DROP`, ...), case-insensitive.
pub fn verdict_marker(verdict: &str) -> Marker {
    match verdict.to_ascii_uppercase().as_str() {
        "FORWARDED" | "ALLOW" | "ALLOWED" => Marker::Ok,
        "DROP" | "DROPPED" | "DENY" | "DENIED" | "BLOCKED" => Marker::Error,
        _ => Marker::Warn,
    }
}

/// The banner and the query summary above the flows. `namespace` and `last` are `None` when the source
/// does not honour them (the gateway returns every namespace over a fixed window).
pub fn flows_header_lines(
    service: Option<&str>,
    namespace: Option<&str>,
    last: Option<&str>,
    source: &str,
    color: bool,
) -> Vec<String> {
    let mut lines = vec![ui::header("Network Flows", color), String::new()];
    if let Some(svc) = service {
        lines.push(ui::kv("Service", svc, 13, color));
    }
    if let Some(ns) = namespace {
        lines.push(ui::kv("Namespace", ns, 13, color));
    }
    if let Some(last) = last {
        lines.push(ui::kv("Time window", last, 13, color));
    }
    lines.push(ui::kv("Source", source, 13, color));
    lines.push(String::new());
    lines
}

/// Lines of the flows table.
pub fn flows_lines(flows: &[serde_json::Value], color: bool) -> Vec<String> {
    let text = |spec: Option<&serde_json::Value>, key: &str, default: &str| -> String {
        spec.and_then(|s| s.get(key))
            .and_then(|v| v.as_str())
            .filter(|v| !v.is_empty())
            .unwrap_or(default)
            .to_string()
    };
    let rows: Vec<Vec<Cell2>> = flows
        .iter()
        .map(|flow| {
            let spec = flow.get("spec");
            let port = spec
                .and_then(|s| s.get("port"))
                .and_then(|v| v.as_u64())
                .map(|p| p.to_string())
                .unwrap_or_else(|| "-".to_string());
            // No verdict means the source does not report one; never show FORWARDED for it.
            let verdict = text(spec, "verdict", "-");
            let marker = verdict_marker(&verdict);
            vec![
                (text(spec, "timestamp", "-"), None),
                (text(spec, "source", "-"), None),
                (text(spec, "destination", "-"), None),
                (text(spec, "protocol", "TCP"), None),
                (port, None),
                (text(spec, "bytes", "0B"), None),
                (text(spec, "latency", "-"), None),
                (verdict, Some(marker)),
            ]
        })
        .collect();
    ui::grid(
        &[
            "TIMESTAMP",
            "SOURCE",
            "DESTINATION",
            "PROTOCOL",
            "PORT",
            "BYTES",
            "LATENCY",
            "VERDICT",
        ],
        &rows,
        color,
    )
}

fn print_flows_table(flows: &[serde_json::Value]) {
    for line in flows_lines(flows, ui::color_enabled()) {
        println!("{line}");
    }
    println!();
    display::print_info(&format!("Total flows: {}", flows.len()));
    println!();
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::commands::network::strip_ansi;
    use serde_json::json;

    fn flow(spec: serde_json::Value) -> serde_json::Value {
        json!({"metadata": {"name": "f"}, "spec": spec})
    }

    fn fixture() -> Vec<serde_json::Value> {
        vec![
            flow(
                json!({"timestamp": "10:00:01", "source": "web", "destination": "api",
                "protocol": "HTTP", "port": 8080, "bytes": "1.2KB", "latency": "3ms",
                "verdict": "FORWARDED"}),
            ),
            flow(json!({"source": "web", "destination": "db", "verdict": "DROP"})),
        ]
    }

    #[test]
    fn flows_table_is_aligned() {
        assert_eq!(
            flows_lines(&fixture(), false),
            vec![
                "TIMESTAMP  SOURCE  DESTINATION  PROTOCOL  PORT  BYTES  LATENCY  VERDICT",
                "10:00:01   web     api          HTTP      8080  1.2KB  3ms      FORWARDED",
                "-          web     db           TCP       -     0B     -        DROP",
            ]
        );
    }

    #[test]
    fn flows_header_and_color() {
        assert_eq!(
            flows_header_lines(
                Some("api"),
                Some("prod"),
                Some("5m"),
                "GryviaServiceGraph edges (kubernetes)",
                false
            ),
            vec![
                "━━━ Network Flows ━━━",
                "",
                "Service      api",
                "Namespace    prod",
                "Time window  5m",
                "Source       GryviaServiceGraph edges (kubernetes)",
                "",
            ]
        );
        assert_eq!(
            flows_header_lines(None, None, None, "gateway (Netra flow history)", false),
            vec![
                "━━━ Network Flows ━━━",
                "",
                "Source       gateway (Netra flow history)",
                "",
            ]
        );
        let plain = flows_lines(&fixture(), false);
        let colored = flows_lines(&fixture(), true);
        assert!(colored.iter().any(|l| l.contains('\x1b')));
        let stripped: Vec<String> = colored.iter().map(|l| strip_ansi(l)).collect();
        assert_eq!(stripped, plain);
    }

    #[tokio::test]
    async fn gateway_flows_are_fetched_and_rendered() {
        use crate::gateway::{mock, GatewayConfig};
        let body = json!({"source": "netra", "items": [
            {"metadata": {"name": "netra-0"}, "spec": {"timestamp": "2026-01-01T00:00:00Z",
                "source": "shop/web-1", "destination": "api", "protocol": "TCP", "port": 8080,
                "bytes": "42", "latency": "1.5ms", "verdict": "FORWARDED"}},
            {"metadata": {"name": "netra-1"}, "spec": {"source": "shop/web-1", "destination": "10.0.0.9",
                "protocol": "TCP", "port": 443, "bytes": "7", "latency": "", "verdict": "DROP"}}
        ]});
        let gw = mock::start(vec![("/api/network/flows", 200, body.to_string())]).await;
        let client = crate::gateway::GatewayClient::new(GatewayConfig {
            base_url: gw.url.clone(),
            api_key: Some("k".into()),
            insecure: false,
            ca_file: None,
            timeout: std::time::Duration::from_secs(5),
        });
        let (items, source) = fetch_gateway(&client).await.unwrap();
        assert_eq!(source, "gateway (Netra flow history)");
        assert_eq!(
            flows_lines(&items, false),
            vec![
                "TIMESTAMP             SOURCE      DESTINATION  PROTOCOL  PORT  BYTES  LATENCY  VERDICT",
                "2026-01-01T00:00:00Z  shop/web-1  api          TCP       8080  42     1.5ms    FORWARDED",
                "-                     shop/web-1  10.0.0.9     TCP       443   7      -        DROP",
            ]
        );
        assert_eq!(gw.auth_seen.lock().unwrap()[0].as_deref(), Some("Bearer k"));
    }

    #[test]
    fn graph_edges_become_flow_items() {
        let g: DynamicObject = serde_json::from_value(json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaServiceGraph",
            "metadata": {"name": "g"},
            "status": {"lastUpdated": "2026-01-01T00:00:00Z", "edges": [
                {"source": "web", "destination": "api", "protocol": "tcp", "port": 8080,
                 "throughput": "2.0 KiB", "latencyP50": "4.3ms"}]}
        }))
        .unwrap();
        let items = graph_edge_items(&[g]);
        assert_eq!(
            flows_lines(&items, false),
            vec![
                "TIMESTAMP             SOURCE  DESTINATION  PROTOCOL  PORT  BYTES    LATENCY  VERDICT",
                "2026-01-01T00:00:00Z  web     api          TCP       8080  2.0 KiB  4.3ms    -",
            ]
        );
    }

    #[test]
    fn verdicts() {
        assert_eq!(verdict_marker("FORWARDED"), Marker::Ok);
        assert_eq!(verdict_marker("drop"), Marker::Error);
        assert_eq!(verdict_marker("AUDIT"), Marker::Warn);
    }
}
