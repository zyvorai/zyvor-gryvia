// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia catalog`: the GPU SKUs a provider offers (GryviaGpuSku, cluster-scoped), with hourly rates.
//!
//! Rates are what the usage meter multiplies GPU hours by; they are estimates, not invoices.

use anyhow::{anyhow, Result};
use kube::api::ListParams;
use serde_json::Value;

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

/// One SKU as far as the catalog view is concerned.
#[derive(Clone, Debug, PartialEq)]
pub struct Sku {
    pub name: String,
    pub gpu_type: String,
    pub gpus_per_unit: u64,
    pub hourly_rate: f64,
    pub currency: String,
    pub spot_discount: u64,
    pub enabled: bool,
}

pub fn sku_from_json(v: &Value) -> Sku {
    let s = |p: &str| {
        v.pointer(p)
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string()
    };
    Sku {
        name: s("/metadata/name"),
        gpu_type: s("/spec/gpuType"),
        gpus_per_unit: v
            .pointer("/spec/gpusPerUnit")
            .and_then(Value::as_u64)
            .unwrap_or(1),
        hourly_rate: v
            .pointer("/spec/hourlyRate")
            .and_then(Value::as_f64)
            .unwrap_or(0.0),
        currency: match s("/spec/currency") {
            c if c.is_empty() => "USD".to_string(),
            c => c,
        },
        spot_discount: v
            .pointer("/spec/spotDiscount")
            .and_then(Value::as_u64)
            .unwrap_or(0),
        enabled: v
            .pointer("/spec/enabled")
            .and_then(Value::as_bool)
            .unwrap_or(true),
    }
}

/// The catalog report: a grid of SKUs and a total line.
pub fn catalog_lines(skus: &[Sku], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("GPU Catalog", color), String::new()];
    if skus.is_empty() {
        lines.push("No SKUs published".to_string());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = skus
        .iter()
        .map(|s| {
            let (enabled, marker) = if s.enabled {
                ("yes", Marker::Ok)
            } else {
                ("no", Marker::Disabled)
            };
            let spot = if s.spot_discount > 0 {
                format!("{}%", s.spot_discount)
            } else {
                "-".to_string()
            };
            vec![
                (s.name.clone(), None),
                (s.gpu_type.clone(), None),
                (s.gpus_per_unit.to_string(), None),
                (format!("{:.2} {}", s.hourly_rate, s.currency), None),
                (spot, None),
                (enabled.to_string(), Some(marker)),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &[
            "NAME",
            "GPU TYPE",
            "GPUs/UNIT",
            "RATE/HOUR",
            "SPOT DISCOUNT",
            "ENABLED",
        ],
        &rows,
        color,
    ));
    lines.push(String::new());
    let enabled = skus.iter().filter(|s| s.enabled).count();
    lines.push(format!("Total: {} SKUs ({} enabled)", skus.len(), enabled));
    lines
}

pub async fn execute(client: &GryviaClient, output: &str) -> Result<()> {
    let api = super::capacity::gvk_api(client, "GryviaGpuSku", "gryviagpuskus");
    let list = api.list(&ListParams::default()).await.map_err(|e| {
        anyhow!(
            "Failed to list SKUs: {}",
            display::cluster_error_text(&e.to_string())
        )
    })?;
    if output != "table" {
        return crate::output::print_serialized(output, &list.items);
    }
    let values: Vec<Value> = list
        .items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .collect();
    let skus: Vec<Sku> = values.iter().map(sku_from_json).collect();
    for line in catalog_lines(&skus, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn fixture() -> Vec<Sku> {
        [
            json!({"metadata": {"name": "h100-8x"}, "spec": {"gpuType": "H100", "gpusPerUnit": 8, "hourlyRate": 24.5, "currency": "USD", "spotDiscount": 60, "enabled": true}}),
            json!({"metadata": {"name": "a100"}, "spec": {"gpuType": "A100", "hourlyRate": 2.0, "enabled": false}}),
        ]
        .iter()
        .map(sku_from_json)
        .collect()
    }

    #[test]
    fn defaults_are_applied() {
        let s = &fixture()[1];
        assert_eq!(s.gpus_per_unit, 1);
        assert_eq!(s.currency, "USD");
        assert_eq!(s.spot_discount, 0);
        assert!(!s.enabled);
        assert!(sku_from_json(&json!({"spec": {}})).enabled);
    }

    #[test]
    fn catalog_report_aligned() {
        let text = catalog_lines(&fixture(), false).join("\n");
        let expected = "\
━━━ GPU Catalog ━━━

NAME     GPU TYPE  GPUs/UNIT  RATE/HOUR  SPOT DISCOUNT  ENABLED
h100-8x  H100      8          24.50 USD  60%            yes
a100     A100      1          2.00 USD   -              no

Total: 2 SKUs (1 enabled)";
        assert_eq!(text, expected);
    }

    #[test]
    fn empty_catalog() {
        let text = catalog_lines(&[], false).join("\n");
        assert!(text.contains("No SKUs published"));
    }
}
