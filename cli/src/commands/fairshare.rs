//! `gryvia tenant fairshare`: lookback GPU-hours vs fairShareWeight recommendations.

use std::collections::BTreeMap;

use anyhow::{anyhow, Result};
use chrono::{Duration, Utc};
use kube::api::ListParams;
use serde::Serialize;
use serde_json::Value;

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2};

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct Row {
    tenant: String,
    weight: i64,
    gpu_hours: f64,
    share_target: f64,
    share_actual: f64,
    recommended_weight: i64,
    delta: i64,
}

fn weight_of(t: &Value) -> i64 {
    t.pointer("/spec/quotas/fairShareWeight")
        .and_then(Value::as_i64)
        .filter(|w| *w > 0)
        .unwrap_or(1)
}

fn tenant_name(t: &Value) -> &str {
    t.pointer("/metadata/name")
        .and_then(Value::as_str)
        .unwrap_or("")
}

fn record_tenant(r: &Value) -> String {
    if let Some(t) = r.pointer("/spec/tenant").and_then(Value::as_str) {
        if !t.is_empty() {
            return t.to_string();
        }
    }
    let ns = r
        .pointer("/metadata/namespace")
        .and_then(Value::as_str)
        .unwrap_or("");
    ns.strip_prefix("tenant-")
        .unwrap_or(ns)
        .to_string()
}

/// Print lookback fairshare for the last `days` days.
pub async fn report(client: &GryviaClient, days: i64, output: &str) -> Result<()> {
    let tenants_api = super::capacity::gvk_api(client, "GryviaTenant", "gryviatenants");
    let tenants = tenants_api
        .list(&ListParams::default())
        .await
        .map_err(|e| {
            anyhow!(
                "Failed to list tenants: {}",
                display::cluster_error_text(&e.to_string())
            )
        })?
        .items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .collect::<Vec<_>>();

    let usage_api = super::capacity::gvk_api(client, "GryviaUsageRecord", "gryviausagerecords");
    let records = usage_api
        .list(&ListParams::default())
        .await
        .map(|l| {
            l.items
                .iter()
                .filter_map(|o| serde_json::to_value(o).ok())
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();

    let cutoff = Utc::now() - Duration::days(days.max(1));
    let mut hours: BTreeMap<String, f64> = BTreeMap::new();
    for r in &records {
        let start = r
            .pointer("/spec/start")
            .and_then(Value::as_str)
            .and_then(|s| chrono::DateTime::parse_from_rfc3339(s).ok())
            .map(|d| d.with_timezone(&Utc));
        if let Some(s) = start {
            if s < cutoff {
                continue;
            }
        }
        let t = record_tenant(r);
        let h = r
            .pointer("/spec/gpuHours")
            .and_then(Value::as_f64)
            .unwrap_or(0.0);
        *hours.entry(t).or_default() += h;
    }

    let mut total_w = 0.0_f64;
    let mut total_h = 0.0_f64;
    let mut base: Vec<(String, i64, f64)> = Vec::new();
    for t in &tenants {
        let name = tenant_name(t).to_string();
        if name.is_empty() {
            continue;
        }
        let w = weight_of(t);
        let h = *hours.get(&name).unwrap_or(&0.0);
        total_w += w as f64;
        total_h += h;
        base.push((name, w, h));
    }

    let mut rows: Vec<Row> = base
        .into_iter()
        .map(|(tenant, weight, gpu_hours)| {
            let share_target = if total_w > 0.0 {
                weight as f64 / total_w
            } else {
                0.0
            };
            let share_actual = if total_h > 0.0 {
                gpu_hours / total_h
            } else {
                0.0
            };
            let ratio = if share_actual > 0.0 && share_target > 0.0 {
                share_target / share_actual
            } else if share_actual == 0.0 && share_target > 0.0 {
                1.5
            } else {
                1.0
            };
            let recommended_weight =
                ((weight as f64) * ratio.clamp(0.5, 2.0)).round().max(1.0) as i64;
            Row {
                tenant,
                weight,
                gpu_hours,
                share_target,
                share_actual,
                recommended_weight,
                delta: recommended_weight - weight,
            }
        })
        .collect();
    rows.sort_by(|a, b| a.tenant.cmp(&b.tenant));

    let payload = serde_json::json!({
        "lookbackDays": days,
        "totalGpuHours": total_h,
        "items": rows,
    });
    if output != "table" {
        return crate::output::print_serialized(output, &payload);
    }

    let color = ui::color_enabled();
    println!("{}", ui::header("Fairshare lookback", color));
    println!();
    let grid_rows: Vec<Vec<Cell2>> = rows
        .iter()
        .map(|r| {
            vec![
                (r.tenant.clone(), None),
                (r.weight.to_string(), None),
                (format!("{:.2}", r.gpu_hours), None),
                (r.recommended_weight.to_string(), None),
                (format!("{:+}", r.delta), None),
            ]
        })
        .collect();
    for line in ui::grid(
        &["TENANT", "WEIGHT", "GPU-HOURS", "RECOMMENDED", "DELTA"],
        &grid_rows,
        color,
    ) {
        println!("{line}");
    }
    println!();
    println!(
        "Lookback {days}d · total GPU-hours {total_h:.2} · recommend-only (gateway apply needs GRYVIA_FAIRSHARE_APPLY=1)"
    );
    Ok(())
}
