// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use kube::api::{Api, ApiResource, GroupVersionKind, ListParams, PostParams};
use kube::core::DynamicObject;
use serde_json::json;

use crate::client::GryviaClient;
use crate::commands::network::severity_marker;
use crate::display;
use crate::gateway::GatewayClient;
use crate::ui::{self, Cell2, Marker};

pub enum SecurityAction {
    Alerts {
        severity: Option<String>,
        alert_type: Option<String>,
        namespace: String,
    },
    Status {
        namespace: String,
    },
    PolicyList {
        namespace: String,
        output: String,
    },
    PolicyCreate {
        name: String,
        namespaces: Vec<String>,
        rules: Vec<String>,
        auto_block: bool,
        namespace: String,
    },
}

pub async fn execute(client: &GryviaClient, action: SecurityAction) -> Result<()> {
    match action {
        SecurityAction::Alerts {
            severity,
            alert_type,
            namespace,
        } => {
            execute_alerts(
                client,
                &namespace,
                severity.as_deref(),
                alert_type.as_deref(),
            )
            .await
        }
        SecurityAction::Status { namespace } => execute_status(client, &namespace).await,
        SecurityAction::PolicyList { namespace, output } => {
            execute_policy_list(client, &namespace, &output).await
        }
        SecurityAction::PolicyCreate {
            name,
            namespaces,
            rules,
            auto_block,
            namespace,
        } => {
            execute_policy_create(client, &namespace, &name, &namespaces, &rules, auto_block).await
        }
    }
}

/// `security alerts` through the gateway (`/api/security/alerts`). Returns `true` when it printed alerts and
/// `false` when the gateway has no per-event source (or no alert), in which case the caller falls back to the
/// counters of the GryviaSecurityPolicy objects. The item keys are read leniently (`severity`, `type` or
/// `eventType`, `description`/`message`/`details`, `process`/`processName`, `detectedAt`/`timestamp`), on
/// either the item or its `spec`; not verified against a gateway that serves per-event alerts.
pub async fn alerts_via_gateway(
    gw: &GatewayClient,
    severity: Option<&str>,
    alert_type: Option<&str>,
) -> Result<bool> {
    let body = gw
        .get_json("/api/security/alerts")
        .await
        .context("could not read security alerts from the gateway")?;
    let rows = gateway_alert_rows(&body, severity, alert_type);
    if rows.is_empty() {
        if body.get("eventSource").and_then(|v| v.as_bool()) == Some(false) {
            display::print_info(
                "The gateway has no per-event alert source; showing the GryviaSecurityPolicy counters instead.",
            );
        }
        return Ok(false);
    }
    let color = ui::color_enabled();
    println!("{}", ui::header("Security Alerts", color));
    println!();
    for line in event_lines(&rows, color) {
        println!("{line}");
    }
    println!();
    display::print_info(&format!("Total alerts: {}", rows.len()));
    println!();
    Ok(true)
}

/// (severity, type, process, details, detected) per gateway alert item, after the filters.
pub fn gateway_alert_rows(
    body: &serde_json::Value,
    severity: Option<&str>,
    alert_type: Option<&str>,
) -> Vec<[String; 5]> {
    let mut rows = Vec::new();
    for item in body
        .get("items")
        .and_then(|v| v.as_array())
        .into_iter()
        .flatten()
    {
        let src = item.get("spec").unwrap_or(item);
        let pick = |keys: &[&str]| -> String {
            keys.iter()
                .find_map(|k| {
                    src.get(*k)
                        .and_then(|v| v.as_str())
                        .filter(|v| !v.is_empty())
                })
                .unwrap_or("-")
                .to_string()
        };
        let row = [
            pick(&["severity"]).to_ascii_lowercase(),
            pick(&["type", "eventType"]),
            pick(&["process", "processName"]),
            pick(&["description", "message", "details"]),
            pick(&["detectedAt", "timestamp"]),
        ];
        if severity.is_some_and(|s| row[0] != s) || alert_type.is_some_and(|t| row[1] != t) {
            continue;
        }
        rows.push(row);
    }
    rows
}

/// Lines of the per-event alerts table.
pub fn event_lines(rows: &[[String; 5]], color: bool) -> Vec<String> {
    let cells: Vec<Vec<Cell2>> = rows
        .iter()
        .map(|r| {
            vec![
                (r[0].clone(), Some(severity_marker(&r[0]))),
                (r[1].clone(), None),
                (r[2].clone(), None),
                (r[3].clone(), None),
                (r[4].clone(), None),
            ]
        })
        .collect();
    ui::grid(
        &["SEVERITY", "TYPE", "PROCESS", "DETAILS", "DETECTED AT"],
        &cells,
        color,
    )
}

async fn execute_alerts(
    client: &GryviaClient,
    namespace: &str,
    severity: Option<&str>,
    alert_type: Option<&str>,
) -> Result<()> {
    let color = ui::color_enabled();
    println!("{}", ui::header("Security Alerts", color));
    println!();

    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaSecurityPolicy",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let policies = match api.list(&ListParams::default()).await {
        Ok(list) => list,
        Err(e) => {
            display::print_warning(&format!("Could not query security policies: {}", e));
            println!();
            return Ok(());
        }
    };

    // Collect all alerts from all policies
    let mut all_alerts: Vec<AlertRow> = Vec::new();

    for policy in &policies.items {
        let status = policy.data.get("status");
        let detection_counts = status
            .and_then(|s| s.get("detectionCounts"))
            .and_then(|v| v.as_object());

        let policy_name = policy.metadata.name.as_deref().unwrap_or("-");

        if let Some(counts) = detection_counts {
            for (det_type, count) in counts {
                let count_val = count.as_i64().unwrap_or(0);
                if count_val == 0 {
                    continue;
                }

                // Apply type filter
                if let Some(filter_type) = alert_type {
                    if det_type != filter_type {
                        continue;
                    }
                }

                let sev = classify_alert_severity(det_type, count_val);

                // Apply severity filter
                if let Some(filter_sev) = severity {
                    if sev != filter_sev {
                        continue;
                    }
                }

                all_alerts.push((
                    sev.to_string(),
                    det_type.to_string(),
                    policy_name.to_string(),
                    format!("{}", count_val),
                    status
                        .and_then(|s| s.get("lastAlert"))
                        .and_then(|v| v.as_str())
                        .unwrap_or("-")
                        .to_string(),
                ));
            }
        }
    }

    if all_alerts.is_empty() {
        println!(
            "{} No security alerts detected.",
            Marker::Ok.paint_with(Marker::Ok.glyph(), color)
        );
        println!();
        return Ok(());
    }

    for line in alerts_lines(&all_alerts, color) {
        println!("{line}");
    }
    println!();
    display::print_info(&format!("Total alerts: {}", all_alerts.len()));
    println!();

    Ok(())
}

type AlertRow = (String, String, String, String, String);

/// Lines of the alerts table (SEVERITY TYPE POLICY COUNT LAST ALERT).
fn alerts_lines(alerts: &[AlertRow], color: bool) -> Vec<String> {
    let rows: Vec<Vec<Cell2>> = alerts
        .iter()
        .map(|(sev, det_type, policy_name, count, last_alert)| {
            vec![
                (sev.clone(), Some(severity_marker(sev))),
                (det_type.clone(), None),
                (policy_name.clone(), None),
                (count.clone(), None),
                (last_alert.clone(), None),
            ]
        })
        .collect();
    ui::grid(
        &["SEVERITY", "TYPE", "POLICY", "COUNT", "LAST ALERT"],
        &rows,
        color,
    )
}

fn classify_alert_severity(alert_type: &str, count: i64) -> &'static str {
    match alert_type {
        "escape" | "privesc" => {
            if count > 5 {
                "critical"
            } else {
                "high"
            }
        }
        "mining" | "exfiltration" => {
            if count > 10 {
                "high"
            } else {
                "medium"
            }
        }
        "driver_fim" => "medium",
        _ => "low",
    }
}

async fn execute_status(client: &GryviaClient, namespace: &str) -> Result<()> {
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaSecurityPolicy",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let policies = match api.list(&ListParams::default()).await {
        Ok(list) => list,
        Err(e) => {
            display::print_warning(&format!("Could not query security policies: {}", e));
            return Ok(());
        }
    };

    let total_policies = policies.items.len();
    let mut total_detections = 0;
    let mut total_alerts = 0;
    let mut active_policies = 0;

    for policy in &policies.items {
        let status = policy.data.get("status");
        let phase = status
            .and_then(|s| s.get("phase"))
            .and_then(|v| v.as_str())
            .unwrap_or("Unknown");

        if phase == "Active" {
            active_policies += 1;
        }

        total_detections += status
            .and_then(|s| s.get("activeDetections"))
            .and_then(|v| v.as_i64())
            .unwrap_or(0);

        total_alerts += status
            .and_then(|s| s.get("alertsTriggered"))
            .and_then(|v| v.as_i64())
            .unwrap_or(0);
    }

    for line in status_lines(
        total_policies,
        active_policies,
        total_detections,
        total_alerts,
        ui::color_enabled(),
    ) {
        println!("{line}");
    }

    Ok(())
}

/// Lines of the security status overview.
fn status_lines(
    total_policies: usize,
    active_policies: usize,
    detections: i64,
    alerts: i64,
    color: bool,
) -> Vec<String> {
    let (marker, label) = if alerts > 50 {
        (Marker::Error, "CRITICAL")
    } else if alerts > 10 {
        (Marker::Warn, "WARNING")
    } else if alerts > 0 {
        (Marker::Warn, "MONITORING")
    } else {
        (Marker::Ok, "SECURE")
    };
    let alerts_marker = if alerts > 0 {
        Marker::Error
    } else {
        Marker::Ok
    };
    let w = 19;
    vec![
        ui::header("Security Status Overview", color),
        String::new(),
        ui::section("Summary", color),
        ui::kv(
            "Security Policies",
            &format!("{total_policies} ({active_policies} active)"),
            w,
            color,
        ),
        ui::kv("Detection Rules", &detections.to_string(), w, color),
        ui::kv(
            "Total Alerts",
            &alerts_marker.paint_with(&alerts.to_string(), color),
            w,
            color,
        ),
        String::new(),
        ui::kv(
            "Security Status",
            &marker.paint_with(&format!("{} {}", marker.glyph(), label), color),
            w,
            color,
        ),
    ]
}

async fn execute_policy_list(client: &GryviaClient, namespace: &str, output: &str) -> Result<()> {
    let color = ui::color_enabled();
    println!("{}", ui::header("Security Policies", color));
    println!();

    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaSecurityPolicy",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let policies = match api.list(&ListParams::default()).await {
        Ok(list) => list,
        Err(e) => {
            display::print_warning(&format!("Could not query security policies: {}", e));
            return Ok(());
        }
    };

    if policies.items.is_empty() {
        println!(
            "{}",
            Marker::Disabled.paint_with("No security policies found.", color)
        );
        println!();
        return Ok(());
    }

    match output {
        "json" | "yaml" => {
            let items: Vec<&serde_json::Value> = policies.items.iter().map(|p| &p.data).collect();
            crate::output::print_serialized(output, &items)?;
        }
        _ => {
            for line in security_policies_lines(&policies.items, color) {
                println!("{line}");
            }
        }
    }

    println!();
    Ok(())
}

/// Lines of the security policies table.
fn security_policies_lines(policies: &[DynamicObject], color: bool) -> Vec<String> {
    let rows: Vec<Vec<Cell2>> = policies
        .iter()
        .map(|policy| {
            let name = policy.metadata.name.as_deref().unwrap_or("-");
            let spec = policy.data.get("spec");
            let status = policy.data.get("status");
            let phase = status
                .and_then(|s| s.get("phase"))
                .and_then(|v| v.as_str())
                .unwrap_or("Unknown");
            let namespaces = spec
                .and_then(|s| s.get("targetNamespaces"))
                .and_then(|v| v.as_array())
                .map(|arr| {
                    arr.iter()
                        .filter_map(|v| v.as_str())
                        .collect::<Vec<_>>()
                        .join(", ")
                })
                .unwrap_or_else(|| "-".to_string());
            let count = |key: &str| {
                status
                    .and_then(|s| s.get(key))
                    .and_then(|v| v.as_i64())
                    .unwrap_or(0)
            };
            let auto_block = spec
                .and_then(|s| s.get("autoBlock"))
                .and_then(|v| v.as_bool())
                .unwrap_or(false);
            let phase_marker = match phase {
                "Active" => Marker::Ok,
                "Error" => Marker::Error,
                _ => Marker::Warn,
            };
            let (block_text, block_marker) = if auto_block {
                ("enabled", Marker::Ok)
            } else {
                ("disabled", Marker::Disabled)
            };
            vec![
                (name.to_string(), None),
                (phase.to_string(), Some(phase_marker)),
                (namespaces, None),
                (count("activeDetections").to_string(), None),
                (count("alertsTriggered").to_string(), None),
                (block_text.to_string(), Some(block_marker)),
            ]
        })
        .collect();
    ui::grid(
        &[
            "NAME",
            "PHASE",
            "NAMESPACES",
            "RULES",
            "ALERTS",
            "AUTO-BLOCK",
        ],
        &rows,
        color,
    )
}

async fn execute_policy_create(
    client: &GryviaClient,
    namespace: &str,
    name: &str,
    target_namespaces: &[String],
    rules: &[String],
    auto_block: bool,
) -> Result<()> {
    let color = ui::color_enabled();
    println!("{}", ui::header("Create Security Policy", color));
    println!();

    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaSecurityPolicy",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let detection_rules: Vec<serde_json::Value> = rules
        .iter()
        .map(|r| {
            json!({
                "type": r,
                "enabled": true,
                "sensitivity": "medium"
            })
        })
        .collect();

    let policy_obj = serde_json::from_value(json!({
        "apiVersion": "gryvia.io/v1alpha1",
        "kind": "GryviaSecurityPolicy",
        "metadata": {
            "name": name,
            "namespace": namespace,
        },
        "spec": {
            "targetNamespaces": target_namespaces,
            "detectionRules": detection_rules,
            "autoBlock": auto_block,
        }
    }))
    .context("Failed to build security policy object")?;

    match api.create(&PostParams::default(), &policy_obj).await {
        Ok(_) => {
            for line in policy_created_lines(name, target_namespaces, rules, auto_block, color) {
                println!("{line}");
            }
        }
        Err(e) => {
            println!(
                "{} Failed to create security policy: {}",
                Marker::Error.paint_with(Marker::Error.glyph(), color),
                e
            );
        }
    }

    println!();
    Ok(())
}

/// Lines confirming a created security policy.
fn policy_created_lines(
    name: &str,
    namespaces: &[String],
    rules: &[String],
    auto_block: bool,
    color: bool,
) -> Vec<String> {
    let (block_text, block_marker) = if auto_block {
        ("enabled", Marker::Ok)
    } else {
        ("disabled", Marker::Disabled)
    };
    vec![
        format!(
            "{} Created security policy: {}",
            Marker::Ok.paint_with(Marker::Ok.glyph(), color),
            name
        ),
        ui::kv("Namespaces", &namespaces.join(", "), 12, color),
        ui::kv("Rules", &rules.join(", "), 12, color),
        ui::kv(
            "Auto-block",
            &block_marker.paint_with(block_text, color),
            12,
            color,
        ),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn gateway_alerts_are_filtered_and_rendered() {
        let body = serde_json::json!({"items": [
            {"spec": {"severity": "HIGH", "type": "crypto_mining", "process": "xmrig", "description": "pool connect", "detectedAt": "2026-01-01T00:00:00Z"}},
            {"severity": "low", "eventType": "suspicious_exec", "processName": "sh", "message": "m", "timestamp": "t"}
        ], "eventSource": true});
        let all = gateway_alert_rows(&body, None, None);
        assert_eq!(all.len(), 2);
        assert_eq!(
            event_lines(&all, false),
            vec![
                "SEVERITY  TYPE             PROCESS  DETAILS       DETECTED AT",
                "high      crypto_mining    xmrig    pool connect  2026-01-01T00:00:00Z",
                "low       suspicious_exec  sh       m             t",
            ]
        );
        assert_eq!(gateway_alert_rows(&body, Some("high"), None).len(), 1);
        assert_eq!(gateway_alert_rows(&body, None, Some("nope")).len(), 0);
        assert!(gateway_alert_rows(
            &serde_json::json!({"items": [], "eventSource": false}),
            None,
            None
        )
        .is_empty());
    }

    use crate::commands::network::strip_ansi;
    use serde_json::json;

    fn policy(name: &str, spec: serde_json::Value, status: serde_json::Value) -> DynamicObject {
        serde_json::from_value(json!({
            "apiVersion": "gryvia.io/v1alpha1",
            "kind": "GryviaSecurityPolicy",
            "metadata": {"name": name},
            "spec": spec,
            "status": status,
        }))
        .unwrap()
    }

    fn alerts() -> Vec<AlertRow> {
        vec![
            (
                "critical",
                "escape",
                "gpu-guard",
                "7",
                "2026-09-29T10:00:00Z",
            ),
            ("medium", "mining", "gpu-guard", "3", "-"),
        ]
        .into_iter()
        .map(|(a, b, c, d, e)| (a.into(), b.into(), c.into(), d.into(), e.into()))
        .collect()
    }

    #[test]
    fn alerts_table_is_aligned() {
        assert_eq!(
            alerts_lines(&alerts(), false),
            vec![
                "SEVERITY  TYPE    POLICY     COUNT  LAST ALERT",
                "critical  escape  gpu-guard  7      2026-09-29T10:00:00Z",
                "medium    mining  gpu-guard  3      -",
            ]
        );
    }

    #[test]
    fn status_renders() {
        assert_eq!(
            status_lines(3, 2, 12, 0, false),
            vec![
                "━━━ Security Status Overview ━━━",
                "",
                "Summary",
                "Security Policies  3 (2 active)",
                "Detection Rules    12",
                "Total Alerts       0",
                "",
                "Security Status    ✓ SECURE",
            ]
        );
    }

    #[test]
    fn policies_table_is_aligned() {
        let items = vec![
            policy(
                "gpu-guard",
                json!({"targetNamespaces": ["ml", "prod"], "autoBlock": true}),
                json!({"phase": "Active", "activeDetections": 4, "alertsTriggered": 10}),
            ),
            policy("empty", json!({}), json!({})),
        ];
        assert_eq!(
            security_policies_lines(&items, false),
            vec![
                "NAME       PHASE    NAMESPACES  RULES  ALERTS  AUTO-BLOCK",
                "gpu-guard  Active   ml, prod    4      10      enabled",
                "empty      Unknown  -           0      0       disabled",
            ]
        );
    }

    #[test]
    fn created_lines_render() {
        let lines = policy_created_lines(
            "p",
            &["ml".to_string()],
            &["escape".to_string(), "mining".to_string()],
            false,
            false,
        );
        assert_eq!(
            lines,
            vec![
                "✓ Created security policy: p",
                "Namespaces  ml",
                "Rules       escape, mining",
                "Auto-block  disabled",
            ]
        );
    }

    #[test]
    fn color_strips_to_plain() {
        let plain = alerts_lines(&alerts(), false);
        let colored = alerts_lines(&alerts(), true);
        assert!(colored.iter().any(|l| l.contains('\x1b')));
        let stripped: Vec<String> = colored.iter().map(|l| strip_ansi(l)).collect();
        assert_eq!(stripped, plain);
        let st = status_lines(1, 1, 1, 60, true);
        assert!(st.iter().any(|l| l.contains('\x1b')));
    }
}
