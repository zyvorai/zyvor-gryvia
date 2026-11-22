// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use kube::api::{Api, ApiResource, GroupVersionKind, ListParams, Patch, PatchParams};
use kube::core::DynamicObject;
use serde_json::json;

use crate::client::GryviaClient;
use crate::commands::flows::verdict_marker;
use crate::display;
use crate::ui::{self, Cell2, Marker};

pub enum PolicyAction {
    Suggest {
        namespace: String,
    },
    Apply {
        policy_name: String,
        namespace: String,
    },
    List {
        namespace: String,
        output: String,
    },
}

pub async fn execute(client: &GryviaClient, action: PolicyAction) -> Result<()> {
    match action {
        PolicyAction::Suggest { namespace } => suggest_policies(client, &namespace).await,
        PolicyAction::Apply {
            policy_name,
            namespace,
        } => apply_policy(client, &policy_name, &namespace).await,
        PolicyAction::List { namespace, output } => {
            list_policies(client, &namespace, &output).await
        }
    }
}

async fn list_policies(client: &GryviaClient, namespace: &str, output: &str) -> Result<()> {
    let color = ui::color_enabled();
    println!("{}", ui::header("Flow Policies", color));
    println!();

    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaFlowPolicy",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let policies = match api.list(&ListParams::default()).await {
        Ok(list) => list,
        Err(e) => {
            display::print_warning(&format!("Could not query flow policies: {}", e));
            println!();
            return Ok(());
        }
    };

    if policies.items.is_empty() {
        println!(
            "{}",
            Marker::Disabled.paint_with("No flow policies found.", color)
        );
        println!();
        return Ok(());
    }

    match output {
        "json" => {
            println!("{}", serde_json::to_string_pretty(&policies)?);
        }
        "yaml" => {
            println!("{}", serde_yaml::to_string(&policies)?);
        }
        _ => {
            print_policies_table(&policies.items);
        }
    }

    Ok(())
}

/// Marker for a policy phase.
fn policy_phase_marker(phase: &str) -> Marker {
    match phase {
        "Enforced" | "Active" => Marker::Ok,
        "Pending" | "Suggested" => Marker::Warn,
        "Error" | "Failed" => Marker::Error,
        _ => Marker::Unknown,
    }
}

/// A string at `spec.<path...>` of a policy, or `default`.
fn spec_text(policy: &DynamicObject, path: &[&str], default: &str) -> String {
    let mut v = policy.data.get("spec");
    for key in path {
        v = v.and_then(|x| x.get(*key));
    }
    v.and_then(|x| x.as_str())
        .filter(|x| !x.is_empty())
        .unwrap_or(default)
        .to_string()
}

/// Lines of the flow policies table. The endpoints are the GryviaFlowPolicy `spec.source.service` and
/// `spec.destination.service` / `.port`.
pub fn policies_lines(policies: &[DynamicObject], color: bool) -> Vec<String> {
    let rows: Vec<Vec<Cell2>> = policies
        .iter()
        .map(|policy| {
            let name = policy.metadata.name.as_deref().unwrap_or("<unknown>");
            let status = policy.data.get("status");
            let port = policy
                .data
                .get("spec")
                .and_then(|s| s.get("destination"))
                .and_then(|d| d.get("port"))
                .and_then(|v| v.as_u64())
                .filter(|p| *p > 0)
                .map(|p| p.to_string())
                .unwrap_or_else(|| "-".to_string());
            let action = spec_text(policy, &["action"], "allow");
            let phase = status
                .and_then(|s| s.get("phase"))
                .and_then(|v| v.as_str())
                .unwrap_or("Pending");
            let matched_flows = status
                .and_then(|s| s.get("matchedFlows"))
                .and_then(|v| v.as_u64())
                .map(|n| n.to_string())
                .unwrap_or_else(|| "-".to_string());
            vec![
                (name.to_string(), None),
                (spec_text(policy, &["source", "service"], "-"), None),
                (spec_text(policy, &["destination", "service"], "-"), None),
                (port, None),
                (action.clone(), Some(verdict_marker(&action))),
                (spec_text(policy, &["intent"], "-"), None),
                (phase.to_string(), Some(policy_phase_marker(phase))),
                (matched_flows, None),
            ]
        })
        .collect();
    ui::grid(
        &[
            "NAME",
            "SOURCE",
            "DESTINATION",
            "PORT",
            "ACTION",
            "INTENT",
            "STATUS",
            "MATCHED FLOWS",
        ],
        &rows,
        color,
    )
}

fn print_policies_table(policies: &[DynamicObject]) {
    for line in policies_lines(policies, ui::color_enabled()) {
        println!("{line}");
    }
    println!();
}

async fn suggest_policies(client: &GryviaClient, namespace: &str) -> Result<()> {
    let color = ui::color_enabled();
    println!("{}", ui::header("Suggested Policies", color));
    println!();
    println!(
        "{}",
        Marker::Disabled.paint_with("Analyzing observed traffic patterns...", color)
    );
    println!();

    // Query for suggested policies (those with phase=Suggested)
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaFlowPolicy",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    let label_selector = "gryvia.io/suggested=true";
    let params = ListParams::default().labels(label_selector);

    let suggestions = match api.list(&params).await {
        Ok(list) => list,
        Err(e) => {
            display::print_warning(&format!("Could not query policy suggestions: {}", e));
            println!();
            return Ok(());
        }
    };

    if suggestions.items.is_empty() {
        println!(
            "{}",
            Marker::Disabled.paint_with(
                "No policy suggestions. They come from a GryviaAutoPolicy (spec.mode suggest) in this namespace once its learning window has passed and the collector has seen traffic.",
                color
            )
        );
        println!();
        return Ok(());
    }

    for line in suggestion_lines(&suggestions.items, color) {
        println!("{line}");
    }

    Ok(())
}

/// Lines of the numbered policy suggestions (GryviaFlowPolicy objects labelled `gryvia.io/suggested=true`,
/// written by the GryviaAutoPolicy controller; the confidence is in the `gryvia.io/confidence` annotation).
pub fn suggestion_lines(suggestions: &[DynamicObject], color: bool) -> Vec<String> {
    let mut lines = Vec::new();
    for (i, suggestion) in suggestions.iter().enumerate() {
        let name = suggestion.metadata.name.as_deref().unwrap_or("<unknown>");
        let port = suggestion
            .data
            .get("spec")
            .and_then(|s| s.get("destination"))
            .and_then(|d| d.get("port"))
            .and_then(|v| v.as_u64())
            .filter(|p| *p > 0)
            .map(|p| p.to_string())
            .unwrap_or_else(|| "*".to_string());
        let action = spec_text(suggestion, &["action"], "allow");
        let confidence = suggestion
            .metadata
            .annotations
            .as_ref()
            .and_then(|a| a.get("gryvia.io/confidence"))
            .and_then(|v| v.parse::<f64>().ok())
            .unwrap_or(0.0);
        let flows = suggestion
            .metadata
            .annotations
            .as_ref()
            .and_then(|a| a.get("gryvia.io/observed-flows"))
            .map(|v| format!(", {v} flows observed"))
            .unwrap_or_default();

        lines.push(format!(
            "{} {} (confidence: {:.0}%{})",
            ui::ansi(&format!("{}.", i + 1), "1", color),
            name,
            confidence * 100.0,
            flows
        ));
        lines.push(format!(
            "   {} → {}  port:{}  action:{}",
            spec_text(suggestion, &["source", "service"], "-"),
            spec_text(suggestion, &["destination", "service"], "-"),
            port,
            verdict_marker(&action).paint_with(&action, color)
        ));
        lines.push(format!(
            "   {}",
            ui::kv(
                "Intent",
                &spec_text(suggestion, &["intent"], "observed traffic pattern"),
                8,
                color
            )
        ));
        lines.push(format!(
            "   {}",
            ui::kv(
                "Apply",
                &Marker::Disabled.paint_with(&format!("gryvia network policy apply {name}"), color),
                8,
                color
            )
        ));
        lines.push(String::new());
    }
    lines
}

async fn apply_policy(client: &GryviaClient, policy_name: &str, namespace: &str) -> Result<()> {
    let color = ui::color_enabled();
    println!(
        "{} Applying policy: {}",
        ui::ansi("→", "1;36", color),
        policy_name
    );

    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaFlowPolicy",
    ));
    let api: Api<DynamicObject> = Api::namespaced_with(client.kube_client.clone(), namespace, &ar);

    // Check policy exists
    let policy = api
        .get(policy_name)
        .await
        .context(format!("Policy '{}' not found", policy_name))?;

    let suggested = policy
        .metadata
        .labels
        .as_ref()
        .and_then(|l| l.get("gryvia.io/suggested"))
        .is_some_and(|v| v == "true");
    let current_phase = policy
        .data
        .get("status")
        .and_then(|s| s.get("phase"))
        .and_then(|v| v.as_str())
        .unwrap_or("Unknown");

    if !suggested {
        display::print_info(&format!(
            "Policy '{}' is not a suggestion (phase: {}); nothing to approve.",
            policy_name, current_phase
        ));
        return Ok(());
    }

    // Approving a suggestion = removing the suggested label. The operator ignores suggested policies and
    // translates a policy without the label into a CiliumNetworkPolicy; the status is its to write.
    let patch = json!({
        "metadata": {
            "labels": {
                "gryvia.io/suggested": null
            },
            "annotations": {
                "gryvia.io/approved-by": "gryvia-cli"
            }
        }
    });

    api.patch(policy_name, &PatchParams::default(), &Patch::Merge(&patch))
        .await
        .context("Failed to approve policy")?;

    println!(
        "{} Policy '{}' approved. The operator will now enforce it; check with: gryvia network policy list",
        Marker::Ok.paint_with(Marker::Ok.glyph(), color),
        policy_name
    );
    println!(
        "  {}",
        Marker::Disabled.paint_with(
            "An allow policy limits the source pods' egress to what it lists; review the whole set before approving.",
            color
        )
    );
    println!();

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::commands::network::strip_ansi;
    use serde_json::json;

    fn policy(name: &str, spec: serde_json::Value, status: serde_json::Value) -> DynamicObject {
        serde_json::from_value(json!({
            "apiVersion": "gryvia.io/v1alpha1",
            "kind": "GryviaFlowPolicy",
            "metadata": {"name": name},
            "spec": spec,
            "status": status,
        }))
        .unwrap()
    }

    fn fixture() -> Vec<DynamicObject> {
        vec![
            policy(
                "web-to-api",
                json!({"source": {"service": "web"}, "destination": {"service": "api", "port": 8080},
                    "action": "allow", "intent": "frontend traffic"}),
                json!({"phase": "Enforced", "matchedFlows": 120}),
            ),
            policy(
                "block-db",
                json!({"source": {"service": "web"}, "destination": {"service": "db"}, "action": "deny"}),
                json!({}),
            ),
        ]
    }

    #[test]
    fn policies_table_is_aligned() {
        assert_eq!(
            policies_lines(&fixture(), false),
            vec![
                "NAME        SOURCE  DESTINATION  PORT  ACTION  INTENT            STATUS    MATCHED FLOWS",
                "web-to-api  web     api          8080  allow   frontend traffic  Enforced  120",
                "block-db    web     db           -     deny    -                 Pending   -",
            ]
        );
    }

    #[test]
    fn suggestions_render() {
        let mut sugg = fixture();
        sugg[0].metadata.annotations = Some(
            [
                ("gryvia.io/confidence".to_string(), "0.90".to_string()),
                ("gryvia.io/observed-flows".to_string(), "120".to_string()),
            ]
            .into_iter()
            .collect(),
        );
        let lines = suggestion_lines(&sugg[..1], false);
        assert_eq!(
            lines,
            vec![
                "1. web-to-api (confidence: 90%, 120 flows observed)",
                "   web → api  port:8080  action:allow",
                "   Intent  frontend traffic",
                "   Apply   gryvia network policy apply web-to-api",
                "",
            ]
        );
    }

    #[test]
    fn color_strips_to_plain() {
        let plain = policies_lines(&fixture(), false);
        let colored = policies_lines(&fixture(), true);
        assert!(colored.iter().any(|l| l.contains('\x1b')));
        let stripped: Vec<String> = colored.iter().map(|l| strip_ansi(l)).collect();
        assert_eq!(stripped, plain);
    }
}
