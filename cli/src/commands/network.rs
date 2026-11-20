// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

use anyhow::Result;
use kube::api::{Api, ApiResource, GroupVersionKind, ListParams};
use kube::core::DynamicObject;

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

pub async fn execute_status(client: &GryviaClient, namespace: &str) -> Result<()> {
    let color = ui::color_enabled();

    // Active flows = distinct edges of the service graphs (there is no per-flow Kubernetes kind)
    let graph_ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaServiceGraph",
    ));
    let graph_api: Api<DynamicObject> =
        Api::namespaced_with(client.kube_client.clone(), namespace, &graph_ar);

    let active_flows = match graph_api.list(&ListParams::default()).await {
        Ok(list) => crate::commands::graph::graphs_to_graph(&list.items).1.len(),
        Err(_) => 0,
    };

    // Query policies
    let policy_ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaFlowPolicy",
    ));
    let policy_api: Api<DynamicObject> =
        Api::namespaced_with(client.kube_client.clone(), namespace, &policy_ar);

    let (total_policies, enforced_policies) = match policy_api.list(&ListParams::default()).await {
        Ok(list) => {
            let enforced = list
                .items
                .iter()
                .filter(|p| {
                    p.data
                        .get("status")
                        .and_then(|s| s.get("phase"))
                        .and_then(|v| v.as_str())
                        .map(|phase| phase == "Enforced" || phase == "Active")
                        .unwrap_or(false)
                })
                .count();
            (list.items.len(), enforced)
        }
        Err(_) => (0, 0),
    };

    // Query anomalies
    let anomaly_ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaNetworkAnomaly",
    ));
    let anomaly_api: Api<DynamicObject> =
        Api::namespaced_with(client.kube_client.clone(), namespace, &anomaly_ar);

    let (total_anomalies, critical_anomalies) = match anomaly_api.list(&ListParams::default()).await
    {
        Ok(list) => {
            let rows = anomaly_rows(&list.items);
            let critical = rows
                .iter()
                .filter(|r| r["spec"]["severity"].as_str() == Some("critical"))
                .count();
            (rows.len(), critical)
        }
        Err(_) => (0, 0),
    };

    // Query trace sessions
    let trace_ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaTraceSession",
    ));
    let trace_api: Api<DynamicObject> =
        Api::namespaced_with(client.kube_client.clone(), namespace, &trace_ar);

    let active_traces = match trace_api.list(&ListParams::default()).await {
        Ok(list) => list
            .items
            .iter()
            .filter(|t| {
                t.data
                    .get("status")
                    .and_then(|s| s.get("phase"))
                    .and_then(|v| v.as_str())
                    .map(|phase| {
                        phase.eq_ignore_ascii_case("active")
                            || phase.eq_ignore_ascii_case("running")
                    })
                    .unwrap_or(false)
            })
            .count(),
        Err(_) => 0,
    };

    let overview = NetworkOverview {
        active_flows,
        total_policies,
        enforced_policies,
        total_anomalies,
        critical_anomalies,
        active_traces,
    };
    for line in overview_lines(&overview, color) {
        println!("{line}");
    }

    Ok(())
}

/// Counts shown on the network overview page.
pub struct NetworkOverview {
    pub active_flows: usize,
    pub total_policies: usize,
    pub enforced_policies: usize,
    pub total_anomalies: usize,
    pub critical_anomalies: usize,
    pub active_traces: usize,
}

/// Lines of the network health overview.
pub fn overview_lines(o: &NetworkOverview, color: bool) -> Vec<String> {
    let anomaly_marker = if o.critical_anomalies > 0 {
        Marker::Error
    } else if o.total_anomalies > 0 {
        Marker::Warn
    } else {
        Marker::Ok
    };
    let anomalies = if o.critical_anomalies > 0 {
        format!("{} ({} critical)", o.total_anomalies, o.critical_anomalies)
    } else {
        o.total_anomalies.to_string()
    };
    let (health_marker, health) = match anomaly_marker {
        Marker::Error => (Marker::Error, "DEGRADED"),
        Marker::Warn => (Marker::Warn, "WARNING"),
        _ => (Marker::Ok, "HEALTHY"),
    };
    let w = 16;
    vec![
        ui::header("Network Health Overview", color),
        String::new(),
        ui::section("Overview", color),
        ui::kv("Active Flows", &o.active_flows.to_string(), w, color),
        ui::kv(
            "Policies",
            &format!("{} ({} enforced)", o.total_policies, o.enforced_policies),
            w,
            color,
        ),
        ui::kv(
            "Anomalies",
            &anomaly_marker.paint_with(&anomalies, color),
            w,
            color,
        ),
        ui::kv("Active Traces", &o.active_traces.to_string(), w, color),
        String::new(),
        ui::kv(
            "Network Status",
            &health_marker.paint_with(&format!("{} {}", health_marker.glyph(), health), color),
            w,
            color,
        ),
    ]
}

/// Marker for an anomaly or alert severity.
pub fn severity_marker(severity: &str) -> Marker {
    match severity.to_ascii_lowercase().as_str() {
        "critical" => Marker::Error,
        "high" | "medium" | "warning" => Marker::Warn,
        "low" | "info" => Marker::Disabled,
        _ => Marker::Unknown,
    }
}

pub async fn execute_anomalies(
    client: &GryviaClient,
    namespace: &str,
    service: Option<&str>,
    severity: Option<&str>,
    output: &str,
) -> Result<()> {
    let color = ui::color_enabled();
    println!("{}", ui::header("Network Anomalies", color));
    println!();

    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaNetworkAnomaly",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let anomalies = match api.list(&ListParams::default()).await {
        Ok(list) => list,
        Err(e) => {
            display::print_warning(&format!(
                "Could not query anomalies: {}",
                display::cluster_error_text(&e.to_string())
            ));
            println!();
            return Ok(());
        }
    };

    let mut rows = anomaly_rows(&anomalies.items);
    if let Some(svc) = service {
        rows.retain(|r| r["spec"]["service"].as_str() == Some(svc));
    }
    if let Some(sev) = severity {
        rows.retain(|r| r["spec"]["severity"].as_str() == Some(sev));
    }

    if rows.is_empty() {
        println!(
            "{} No anomalies detected.",
            Marker::Ok.paint_with(Marker::Ok.glyph(), color)
        );
        println!();
        return Ok(());
    }

    match output {
        "json" | "yaml" => {
            crate::output::print_serialized(output, &rows)?;
        }
        _ => {
            print_anomalies_table(&rows);
        }
    }

    Ok(())
}

/// One row per anomaly in the `status.anomalies` of GryviaNetworkAnomaly objects, shaped like the gateway's
/// `/api/network/anomalies` items: `{"spec": {severity, service, type, description, detectedAt}}`.
pub fn anomaly_rows(objects: &[DynamicObject]) -> Vec<serde_json::Value> {
    let mut rows = Vec::new();
    for o in objects {
        let service = o
            .data
            .get("spec")
            .and_then(|s| s.get("targetService"))
            .and_then(|v| v.as_str())
            .unwrap_or("");
        for a in o
            .data
            .get("status")
            .and_then(|s| s.get("anomalies"))
            .and_then(|v| v.as_array())
            .into_iter()
            .flatten()
        {
            let text = |k: &str| a.get(k).and_then(|v| v.as_str()).unwrap_or("");
            rows.push(serde_json::json!({"spec": {
                "severity": text("severity").to_ascii_lowercase(),
                "service": service,
                "type": text("type"),
                "description": text("description"),
                "detectedAt": text("detected"),
            }}));
        }
    }
    rows
}

/// Lines of the anomalies table (SEVERITY SERVICE TYPE DESCRIPTION DETECTED AT).
pub fn anomalies_lines(anomalies: &[serde_json::Value], color: bool) -> Vec<String> {
    let text = |spec: Option<&serde_json::Value>, key: &str| -> String {
        spec.and_then(|s| s.get(key))
            .and_then(|v| v.as_str())
            .filter(|v| !v.is_empty())
            .unwrap_or("-")
            .to_string()
    };
    let rows: Vec<Vec<Cell2>> = anomalies
        .iter()
        .map(|anomaly| {
            let spec = anomaly.get("spec");
            let severity = spec
                .and_then(|s| s.get("severity"))
                .and_then(|v| v.as_str())
                .filter(|v| !v.is_empty())
                .unwrap_or("unknown");
            vec![
                (severity.to_string(), Some(severity_marker(severity))),
                (text(spec, "service"), None),
                (text(spec, "type"), None),
                (text(spec, "description"), None),
                (text(spec, "detectedAt"), None),
            ]
        })
        .collect();
    ui::grid(
        &["SEVERITY", "SERVICE", "TYPE", "DESCRIPTION", "DETECTED AT"],
        &rows,
        color,
    )
}

fn print_anomalies_table(anomalies: &[serde_json::Value]) {
    for line in anomalies_lines(anomalies, ui::color_enabled()) {
        println!("{line}");
    }
    println!();
    display::print_info(&format!("Total anomalies: {}", anomalies.len()));
    println!();
}

/// Remove ANSI color codes, for tests that compare colored and plain output.
#[cfg(test)]
pub(crate) fn strip_ansi(s: &str) -> String {
    let mut out = String::new();
    let mut chars = s.chars();
    while let Some(c) = chars.next() {
        if c == '\x1b' {
            for n in chars.by_ref() {
                if n == 'm' {
                    break;
                }
            }
        } else {
            out.push(c);
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use strip_ansi as strip;

    fn anomaly_object(target: &str, anomalies: serde_json::Value) -> DynamicObject {
        serde_json::from_value(json!({
            "apiVersion": "gryvia.io/v1alpha1",
            "kind": "GryviaNetworkAnomaly",
            "metadata": {"name": "a"},
            "spec": {"targetService": target},
            "status": {"anomalies": anomalies},
        }))
        .unwrap()
    }

    #[test]
    fn anomalies_table_is_aligned() {
        let rows = anomaly_rows(&[
            anomaly_object(
                "api",
                json!([{"severity": "critical", "type": "latency_spike",
                "description": "p99 above 2s", "detected": "2026-09-29T10:00:00Z"}]),
            ),
            anomaly_object("db", json!([{"severity": "Low"}])),
        ]);
        assert_eq!(rows.len(), 2);
        assert_eq!(
            anomalies_lines(&rows, false),
            vec![
                "SEVERITY  SERVICE  TYPE           DESCRIPTION   DETECTED AT",
                "critical  api      latency_spike  p99 above 2s  2026-09-29T10:00:00Z",
                "low       db       -              -             -",
            ]
        );
    }

    #[test]
    fn overview_renders_and_colors() {
        let o = NetworkOverview {
            active_flows: 12,
            total_policies: 4,
            enforced_policies: 3,
            total_anomalies: 2,
            critical_anomalies: 1,
            active_traces: 0,
        };
        let plain = overview_lines(&o, false);
        assert_eq!(
            plain,
            vec![
                "━━━ Network Health Overview ━━━",
                "",
                "Overview",
                "Active Flows    12",
                "Policies        4 (3 enforced)",
                "Anomalies       2 (1 critical)",
                "Active Traces   0",
                "",
                "Network Status  ✗ DEGRADED",
            ]
        );
        let colored = overview_lines(&o, true);
        assert!(colored.iter().any(|l| l.contains('\x1b')));
        let stripped: Vec<String> = colored.iter().map(|l| strip(l)).collect();
        assert_eq!(stripped, plain);
    }

    #[test]
    fn severity_markers() {
        assert_eq!(severity_marker("critical"), Marker::Error);
        assert_eq!(severity_marker("high"), Marker::Warn);
        assert_eq!(severity_marker("info"), Marker::Disabled);
        assert_eq!(severity_marker("weird"), Marker::Unknown);
    }
}
