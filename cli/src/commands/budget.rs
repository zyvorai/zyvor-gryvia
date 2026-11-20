// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia budget`: GryviaBudget objects (cluster-scoped) with spend against limits, read through the
//! Kubernetes API like `gryvia quota`. Spend is an estimate from usage records, not an invoice.

use anyhow::{anyhow, Result};
use kube::api::ListParams;
use serde_json::Value;

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

fn str_at<'a>(v: &'a Value, ptr: &str) -> &'a str {
    v.pointer(ptr).and_then(Value::as_str).unwrap_or("")
}

fn f64_at(v: &Value, ptr: &str) -> f64 {
    v.pointer(ptr).and_then(Value::as_f64).unwrap_or(0.0)
}

fn state_marker(state: &str) -> Marker {
    match state {
        "active" => Marker::Ok,
        "warning" => Marker::Warn,
        "exceeded" | "blocked" => Marker::Error,
        _ => Marker::Unknown,
    }
}

/// The budget table with a total line.
pub fn budget_lines(items: &[Value], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("Budgets", color), String::new()];
    if items.is_empty() {
        lines.push("No budgets found".to_string());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = items
        .iter()
        .map(|b| {
            let state = match str_at(b, "/status/state") {
                "" => "-",
                s => s,
            };
            let limit = f64_at(b, "/spec/limits/costUSD");
            let spent = f64_at(b, "/status/usage/costUSD");
            let percent = match b
                .pointer("/status/utilization/costPercent")
                .and_then(Value::as_f64)
            {
                Some(p) => p,
                None if limit > 0.0 => spent / limit * 100.0,
                None => 0.0,
            };
            let limit_text = if limit > 0.0 {
                format!("${limit:.2}")
            } else {
                "-".to_string()
            };
            let enforcement = if b
                .pointer("/spec/enforcement/enabled")
                .and_then(Value::as_bool)
                .unwrap_or(false)
            {
                match str_at(b, "/spec/enforcement/action") {
                    "" => "enabled",
                    a => a,
                }
            } else {
                "alerts only"
            };
            vec![
                (
                    format!(
                        "{}/{}",
                        str_at(b, "/spec/scope/type"),
                        str_at(b, "/spec/scope/name")
                    ),
                    None,
                ),
                (str_at(b, "/spec/period/type").to_string(), None),
                (format!("${spent:.2} / {limit_text}"), None),
                (format!("{percent:.1}%"), None),
                (state.to_string(), Some(state_marker(state))),
                (enforcement.to_string(), None),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &[
            "SCOPE",
            "PERIOD",
            "SPENT / LIMIT",
            "USED",
            "STATE",
            "ENFORCEMENT",
        ],
        &rows,
        color,
    ));
    lines.push(String::new());
    lines.push(format!("Total: {} budgets", items.len()));
    lines
}

pub async fn execute(client: &GryviaClient, scope: Option<String>, output: &str) -> Result<()> {
    let api = super::capacity::gvk_api(client, "GryviaBudget", "gryviabudgets");
    let items = api.list(&ListParams::default()).await.map_err(|e| {
        anyhow!(
            "Failed to list budgets: {}",
            display::cluster_error_text(&e.to_string())
        )
    })?;
    let items: Vec<_> = items
        .items
        .into_iter()
        .filter(|o| match &scope {
            Some(s) => {
                let v = serde_json::to_value(o).unwrap_or_default();
                str_at(&v, "/spec/scope/name") == s
            }
            None => true,
        })
        .collect();
    if output != "table" {
        return crate::output::print_serialized(output, &items);
    }
    let values: Vec<Value> = items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .collect();
    for line in budget_lines(&values, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn budget_report_aligned() {
        let items = vec![
            json!({"metadata": {"name": "ml"}, "spec": {"scope": {"type": "team", "name": "ml"},
                "period": {"type": "monthly"}, "limits": {"costUSD": 1000.0},
                "enforcement": {"enabled": true, "action": "block"}},
                "status": {"state": "warning", "usage": {"costUSD": 850.5}, "utilization": {"costPercent": 85.05}}}),
            json!({"metadata": {"name": "x"}, "spec": {"scope": {"type": "namespace", "name": "x"},
                "period": {"type": "daily"}, "limits": {"costUSD": 10.0}},
                "status": {"usage": {"costUSD": 2.5}}}),
        ];
        let text = budget_lines(&items, false).join("\n");
        let expected = "\
━━━ Budgets ━━━

SCOPE        PERIOD   SPENT / LIMIT       USED   STATE    ENFORCEMENT
team/ml      monthly  $850.50 / $1000.00  85.0%  warning  block
namespace/x  daily    $2.50 / $10.00      25.0%  -        alerts only

Total: 2 budgets";
        assert_eq!(text, expected);
    }

    #[test]
    fn empty() {
        assert!(budget_lines(&[], false)
            .join("\n")
            .contains("No budgets found"));
    }
}
