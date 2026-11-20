// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

use anyhow::{Context, Result};
use k8s_openapi::api::core::v1::ConfigMap;
use kube::api::{Api, ApiResource, GroupVersionKind, PostParams};
use kube::core::DynamicObject;
use serde_json::json;
use tokio::time::{sleep, Duration};

use crate::client::GryviaClient;
use crate::commands::flows::verdict_marker;
use crate::display;
use crate::ui::{self, Cell2, Marker};

pub async fn execute(
    client: &GryviaClient,
    service: &str,
    duration: &str,
    level: &str,
    namespace: &str,
    follow: bool,
) -> Result<()> {
    let color = ui::color_enabled();
    for line in trace_header_lines(service, duration, level, namespace, color) {
        println!("{line}");
    }

    // Create trace session via CRD
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaTraceSession",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let session_name = format!("trace-{}-{}", service, chrono::Utc::now().timestamp());
    let trace_obj = serde_json::from_value(json!({
        "apiVersion": "gryvia.io/v1alpha1",
        "kind": "GryviaTraceSession",
        "metadata": {
            "name": session_name,
            "namespace": namespace,
        },
        "spec": {
            "service": service,
            "duration": duration,
            "level": level,
            "namespace": namespace,
        }
    }))
    .context("Failed to build trace session object")?;

    match api.create(&PostParams::default(), &trace_obj).await {
        Ok(_) => {
            println!(
                "{} Created trace session: {}",
                Marker::Ok.paint_with(Marker::Ok.glyph(), color),
                session_name
            );
        }
        Err(e) => {
            display::print_warning(&format!(
                "Could not create trace session: {}",
                display::cluster_error_text(&e.to_string())
            ));
            println!();
            return Ok(());
        }
    }

    println!();
    println!("{}", ui::section("Captured Flows", color));
    println!();

    if follow {
        // Poll for results in follow mode
        loop {
            let flows = fetch_flows(client, namespace, &session_name).await;
            print_flows(&flows, color);

            // Check if trace session has completed
            if let Ok(session) = api.get(&session_name).await {
                let phase = session
                    .data
                    .get("status")
                    .and_then(|s| s.get("phase"))
                    .and_then(|v| v.as_str())
                    .unwrap_or("active");

                if phase == "completed" || phase == "expired" {
                    println!();
                    display::print_info(&format!(
                        "Trace session {} ({})",
                        phase.to_lowercase(),
                        session_name
                    ));
                    break;
                }
            }

            sleep(Duration::from_secs(2)).await;
        }
    } else {
        // One-shot: fetch current flows
        let flows = fetch_flows(client, namespace, &session_name).await;
        if flows.is_empty() {
            println!(
                "{}",
                Marker::Disabled.paint_with(
                    "No flows captured yet. The operator captures from Netra (GRYVIA_NETRA_URL on the operator); check `kubectl get gryviatracesession -o yaml` for the SourceAvailable condition. Use --follow to keep polling.",
                    color,
                )
            );
        } else {
            print_flows(&flows, color);
        }
    }

    Ok(())
}

pub struct FlowEntry {
    timestamp: String,
    source: String,
    destination: String,
    protocol: String,
    bytes: String,
    latency: String,
    verdict: String,
    policy: Option<String>,
}

/// The flows the operator wrote to the session's result ConfigMap (`trace-<session>`, key `flows`).
async fn fetch_flows(client: &GryviaClient, namespace: &str, session: &str) -> Vec<FlowEntry> {
    let api: Api<ConfigMap> = Api::namespaced(client.kube_client.clone(), namespace);
    match api.get(&format!("trace-{session}")).await {
        Ok(cm) => cm
            .data
            .and_then(|d| d.get("flows").cloned())
            .map(|raw| parse_flows(&raw))
            .unwrap_or_default(),
        Err(_) => Vec::new(),
    }
}

/// Parse the operator's captured-flow JSON (`timestamp, source, destination, port, protocol, verdict, bytes`).
pub fn parse_flows(raw: &str) -> Vec<FlowEntry> {
    let values: Vec<serde_json::Value> = serde_json::from_str(raw).unwrap_or_default();
    values
        .iter()
        .map(|f| {
            let text = |k: &str, d: &str| {
                f.get(k)
                    .and_then(|v| v.as_str())
                    .filter(|v| !v.is_empty())
                    .unwrap_or(d)
                    .to_string()
            };
            let protocol = match f.get("port").and_then(|v| v.as_u64()) {
                Some(p) if p > 0 => format!("{}/{p}", text("protocol", "TCP")),
                _ => text("protocol", "TCP"),
            };
            FlowEntry {
                timestamp: text("timestamp", "-"),
                source: text("source", "unknown"),
                destination: text("destination", "unknown"),
                protocol,
                bytes: f
                    .get("bytes")
                    .and_then(|v| v.as_i64())
                    .map(|b| format!("{b}B"))
                    .unwrap_or_else(|| "0B".to_string()),
                latency: "-".to_string(),
                verdict: text("verdict", "-"),
                policy: None,
            }
        })
        .collect()
}

fn print_flows(flows: &[FlowEntry], color: bool) {
    for line in flow_lines(flows, color) {
        println!("{line}");
    }
}

/// The banner and the trace parameters.
pub fn trace_header_lines(
    service: &str,
    duration: &str,
    level: &str,
    namespace: &str,
    color: bool,
) -> Vec<String> {
    vec![
        ui::header(&format!("Trace: {service}"), color),
        String::new(),
        ui::kv("Service", service, 11, color),
        ui::kv("Duration", duration, 11, color),
        ui::kv("Level", level, 11, color),
        ui::kv("Namespace", namespace, 11, color),
        String::new(),
    ]
}

/// Lines of the captured-flows table.
fn flow_lines(flows: &[FlowEntry], color: bool) -> Vec<String> {
    let rows: Vec<Vec<Cell2>> = flows
        .iter()
        .map(|flow| {
            let marker = verdict_marker(&flow.verdict);
            let verdict = match (marker, flow.policy.as_deref()) {
                (Marker::Error, Some(p)) => format!("{} (policy: {})", flow.verdict, p),
                _ => flow.verdict.clone(),
            };
            vec![
                (flow.timestamp.clone(), None),
                (flow.source.clone(), None),
                (flow.destination.clone(), None),
                (flow.protocol.clone(), None),
                (flow.bytes.clone(), None),
                (flow.latency.clone(), None),
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
            "BYTES",
            "LATENCY",
            "VERDICT",
        ],
        &rows,
        color,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::commands::network::strip_ansi;

    fn fixture() -> Vec<FlowEntry> {
        vec![
            FlowEntry {
                timestamp: "10:00:01".into(),
                source: "web".into(),
                destination: "api".into(),
                protocol: "HTTP".into(),
                bytes: "1.2KB".into(),
                latency: "3ms".into(),
                verdict: "FORWARDED".into(),
                policy: None,
            },
            FlowEntry {
                timestamp: "10:00:02".into(),
                source: "web".into(),
                destination: "db".into(),
                protocol: "TCP".into(),
                bytes: "0B".into(),
                latency: "-".into(),
                verdict: "DROP".into(),
                policy: Some("deny-db".into()),
            },
        ]
    }

    #[test]
    fn flows_render_aligned() {
        assert_eq!(
            flow_lines(&fixture(), false),
            vec![
                "TIMESTAMP  SOURCE  DESTINATION  PROTOCOL  BYTES  LATENCY  VERDICT",
                "10:00:01   web     api          HTTP      1.2KB  3ms      FORWARDED",
                "10:00:02   web     db           TCP       0B     -        DROP (policy: deny-db)",
            ]
        );
    }

    #[test]
    fn operator_flows_parse() {
        let raw = r#"[{"timestamp":"2026-01-01T00:00:00Z","source":"shop/web-1","destination":"api","port":8080,
            "protocol":"TCP","verdict":"DROP","bytes":42}]"#;
        assert_eq!(
            flow_lines(&parse_flows(raw), false),
            vec![
                "TIMESTAMP             SOURCE      DESTINATION  PROTOCOL  BYTES  LATENCY  VERDICT",
                "2026-01-01T00:00:00Z  shop/web-1  api          TCP/8080  42B    -        DROP",
            ]
        );
        assert!(parse_flows("not json").is_empty());
    }

    #[test]
    fn header_and_color() {
        assert_eq!(
            trace_header_lines("api", "30s", "l4", "prod", false),
            vec![
                "━━━ Trace: api ━━━",
                "",
                "Service    api",
                "Duration   30s",
                "Level      l4",
                "Namespace  prod",
                "",
            ]
        );
        let plain = flow_lines(&fixture(), false);
        let colored = flow_lines(&fixture(), true);
        assert!(colored.iter().any(|l| l.contains('\x1b')));
        let stripped: Vec<String> = colored.iter().map(|l| strip_ansi(l)).collect();
        assert_eq!(stripped, plain);
    }
}
