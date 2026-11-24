// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia waste`: allocated GPU-hours against measured utilization, per tenant, via the gateway's
//! `/api/waste`.

use anyhow::{anyhow, Result};
use serde_json::Value;

use crate::gateway::GatewayClient;
use crate::output;
use crate::ui::{self, Cell2};

fn num(v: &Value, key: &str) -> Option<f64> {
    v.get(key).and_then(Value::as_f64)
}

fn fmt_opt(v: Option<f64>, suffix: &str) -> String {
    v.map(|x| format!("{x:.2}{suffix}"))
        .unwrap_or_else(|| "-".to_string())
}

/// The table rows of a `/api/waste` body: tenant, GPU-hours, utilization, wasted cost, verdict.
pub fn table_rows(body: &Value) -> Vec<Vec<Cell2>> {
    body.get("items")
        .and_then(Value::as_array)
        .map(|items| {
            items
                .iter()
                .map(|i| {
                    let cur = i.get("currency").and_then(Value::as_str).unwrap_or("");
                    vec![
                        (
                            i.get("tenant")
                                .and_then(Value::as_str)
                                .unwrap_or("")
                                .to_string(),
                            None,
                        ),
                        (fmt_opt(num(i, "gpuHours"), ""), None),
                        (fmt_opt(num(i, "utilizationPct"), "%"), None),
                        (
                            match num(i, "wastedCost") {
                                Some(c) => format!("{c:.2} {cur}"),
                                None => "-".to_string(),
                            },
                            None,
                        ),
                        (
                            i.get("verdict")
                                .and_then(Value::as_str)
                                .unwrap_or("")
                                .to_string(),
                            None,
                        ),
                    ]
                })
                .collect()
        })
        .unwrap_or_default()
}

pub async fn report(
    gateway: &GatewayClient,
    days: u32,
    low_util_pct: f64,
    output: &str,
) -> Result<()> {
    let path = format!("/api/waste?days={days}&lowUtilPct={low_util_pct}");
    let v = gateway
        .get_json(&path)
        .await
        .map_err(|e| anyhow!("waste: {e}"))?;
    if output != "table" {
        return output::print_serialized(output, &v);
    }
    let color = ui::color_enabled();
    println!("{}", ui::header("GPU waste", color));
    println!();
    for line in ui::grid(
        &[
            "TENANT",
            "GPU-HOURS",
            "UTILIZATION",
            "WASTED COST",
            "VERDICT",
        ],
        &table_rows(&v),
        color,
    ) {
        println!("{line}");
    }
    println!();
    let t = v.get("totals").cloned().unwrap_or(Value::Null);
    println!(
        "Last {days}d · {} GPU-hours metered · utilization {}",
        fmt_opt(num(&t, "gpuHours"), ""),
        fmt_opt(num(&t, "utilizationPct"), "%")
    );
    if v.get("telemetry").and_then(Value::as_bool) != Some(true) {
        println!(
            "No utilization telemetry: the gateway could not read DCGM metrics from Prometheus."
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn rows_show_measured_and_missing_telemetry() {
        let body = json!({"items": [
            {"tenant": "beta", "gpuHours": 10.0, "utilizationPct": 20.0, "wastedCost": 32.0, "currency": "USD", "verdict": "underused"},
            {"tenant": "gamma", "gpuHours": 4.0, "utilizationPct": null, "wastedCost": null, "currency": "USD", "verdict": "noTelemetry"}
        ]});
        let rows = table_rows(&body);
        let text: Vec<Vec<&str>> = rows
            .iter()
            .map(|r| r.iter().map(|c| c.0.as_str()).collect())
            .collect();
        assert_eq!(
            text[0],
            vec!["beta", "10.00", "20.00%", "32.00 USD", "underused"]
        );
        assert_eq!(text[1], vec!["gamma", "4.00", "-", "-", "noTelemetry"]);
    }

    #[test]
    fn empty_body_has_no_rows() {
        assert!(table_rows(&json!({})).is_empty());
    }
}
