// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia usage --network`: measured tenant network egress from GryviaNetworkUsageRecord objects.
//!
//! Aggregation is client-side and follows the gateway (`routers/netusage.py`): one source (`collector`), records
//! filtered on `spec.hour`, duplicate fragments of the same (node, tenant, hour, peerClass, zoneClass) counted
//! once (the larger egress wins), only egress is priced, and zone classes without a rate (unknown-zone,
//! same-node) are shown but never charged. Costs are estimates from byte counters; nothing is invoiced.

use std::collections::BTreeMap;

use anyhow::{anyhow, Result};
use chrono::{DateTime, Utc};
use kube::api::ListParams;
use serde::Serialize;
use serde_json::Value;

use super::usage::{csv_cell, parse_bound, GroupBy, UsageFormat};
use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2};

const GB: f64 = 1_000_000_000.0;

/// A usage fragment as far as aggregation is concerned.
#[derive(Clone, Debug, PartialEq)]
pub struct NetRecord {
    pub tenant: String,
    pub node: String,
    pub source: String,
    pub hour: Option<DateTime<Utc>>,
    pub peer_class: String,
    pub zone_class: String,
    pub egress: f64,
    pub ingress: f64,
    pub is_final: bool,
}

/// The provider's price per GB (`GryviaNetworkRate`).
#[derive(Clone, Debug, PartialEq)]
pub struct Rates {
    pub same_zone: f64,
    pub cross_zone: f64,
    pub internet: f64,
    pub currency: String,
}

fn text(v: &Value, ptr: &str) -> String {
    v.pointer(ptr)
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string()
}

fn num(v: &Value, ptr: &str) -> f64 {
    v.pointer(ptr).and_then(Value::as_f64).unwrap_or(0.0)
}

pub fn record_from_json(v: &Value) -> NetRecord {
    let ns = text(v, "/metadata/namespace");
    let tenant = match text(v, "/spec/tenant") {
        t if !t.is_empty() => t,
        _ => ns.strip_prefix("tenant-").unwrap_or(&ns).to_string(),
    };
    NetRecord {
        tenant,
        node: text(v, "/spec/node"),
        source: match text(v, "/spec/source") {
            s if s.is_empty() => "collector".into(),
            s => s,
        },
        hour: DateTime::parse_from_rfc3339(&text(v, "/spec/hour"))
            .ok()
            .map(|d| d.with_timezone(&Utc)),
        peer_class: text(v, "/spec/peerClass"),
        zone_class: text(v, "/spec/zoneClass"),
        egress: num(v, "/spec/egressBytes"),
        ingress: num(v, "/spec/ingressBytes"),
        is_final: v
            .pointer("/spec/final")
            .and_then(Value::as_bool)
            .unwrap_or(false),
    }
}

pub fn rates_from_json(v: &Value) -> Rates {
    Rates {
        same_zone: num(v, "/spec/sameZone"),
        cross_zone: num(v, "/spec/crossZone"),
        internet: num(v, "/spec/internetEgress"),
        currency: match text(v, "/spec/currency") {
            c if c.is_empty() => "USD".into(),
            c => c,
        },
    }
}

/// Egress of one zone class in money, None when the class is not priced.
pub fn price(zone: &str, egress_bytes: f64, rates: Option<&Rates>) -> Option<f64> {
    let r = rates?;
    let per_gb = match zone {
        "same-zone" => r.same_zone,
        "cross-zone" => r.cross_zone,
        "internet" => r.internet,
        _ => return None,
    };
    Some(egress_bytes / GB * per_gb)
}

/// One source, one tenant (optional), hour in `[lo, hi)`, duplicates counted once.
pub fn clean(
    records: &[NetRecord],
    tenant: Option<&str>,
    lo: Option<DateTime<Utc>>,
    hi: Option<DateTime<Utc>>,
) -> Vec<NetRecord> {
    let mut best: BTreeMap<(String, String, DateTime<Utc>, String, String), NetRecord> =
        BTreeMap::new();
    for r in records {
        let Some(h) = r.hour else { continue };
        if r.source != "collector"
            || tenant.is_some_and(|t| t != r.tenant)
            || lo.is_some_and(|l| h < l)
            || hi.is_some_and(|x| h >= x)
        {
            continue;
        }
        let key = (
            r.node.clone(),
            r.tenant.clone(),
            h,
            r.peer_class.clone(),
            r.zone_class.clone(),
        );
        match best.get(&key) {
            Some(old) if old.egress >= r.egress => {}
            _ => {
                best.insert(key, r.clone());
            }
        }
    }
    best.into_values().collect()
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct NetRow {
    pub key: String,
    #[serde(rename = "egressBytes")]
    pub egress_bytes: u64,
    #[serde(rename = "ingressBytes")]
    pub ingress_bytes: u64,
    pub records: usize,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cost: Option<f64>,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct NetReport {
    #[serde(rename = "groupBy")]
    pub group_by: String,
    pub source: String,
    pub billing: String,
    pub items: Vec<NetRow>,
    pub totals: NetRow,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub currency: Option<String>,
    /// True while an included record is not final.
    pub open: bool,
}

fn round4(x: f64) -> f64 {
    (x * 10_000.0).round() / 10_000.0
}

fn key_of(r: &NetRecord, by: GroupBy) -> String {
    match by {
        GroupBy::PeerClass => r.peer_class.clone(),
        GroupBy::ZoneClass => r.zone_class.clone(),
        GroupBy::Day => r
            .hour
            .map(|h| h.date_naive().to_string())
            .unwrap_or_else(|| "unknown".into()),
        _ => r.tenant.clone(),
    }
}

pub fn aggregate(records: &[NetRecord], by: GroupBy, rates: Option<&Rates>) -> NetReport {
    let mut groups: BTreeMap<String, (f64, f64, usize, f64)> = BTreeMap::new();
    let mut all = (0.0, 0.0, 0usize, 0.0);
    let mut open = false;
    for r in records {
        let c = price(&r.zone_class, r.egress, rates).unwrap_or(0.0);
        let g = groups.entry(key_of(r, by)).or_default();
        for a in [g, &mut all] {
            a.0 += r.egress;
            a.1 += r.ingress;
            a.2 += 1;
            a.3 += c;
        }
        open |= !r.is_final;
    }
    let row = |key: String, a: (f64, f64, usize, f64)| NetRow {
        key,
        egress_bytes: a.0 as u64,
        ingress_bytes: a.1 as u64,
        records: a.2,
        cost: rates.map(|_| round4(a.3)),
    };
    let mut items: Vec<NetRow> = groups.into_iter().map(|(k, a)| row(k, a)).collect();
    if by != GroupBy::Day {
        items.sort_by_key(|a| std::cmp::Reverse(a.egress_bytes));
    }
    NetReport {
        group_by: match by {
            GroupBy::PeerClass => "peerClass",
            GroupBy::ZoneClass => "zoneClass",
            GroupBy::Day => "day",
            _ => "tenant",
        }
        .to_string(),
        source: "collector".into(),
        billing: "egress-only".into(),
        items,
        totals: row("total".into(), all),
        currency: rates.map(|r| r.currency.clone()),
        open,
    }
}

fn gb(bytes: u64) -> String {
    format!("{:.3}", bytes as f64 / GB)
}

pub fn csv_lines(report: &NetReport) -> Vec<String> {
    let line = |cells: Vec<String>| {
        cells
            .iter()
            .map(|c| csv_cell(c))
            .collect::<Vec<_>>()
            .join(",")
    };
    let cur = report.currency.clone().unwrap_or_default();
    let mut out = vec![line(vec![
        report.group_by.clone(),
        "egress_bytes".into(),
        "ingress_bytes".into(),
        "cost".into(),
        "currency".into(),
    ])];
    for r in report.items.iter().chain(std::iter::once(&report.totals)) {
        out.push(line(vec![
            r.key.clone(),
            r.egress_bytes.to_string(),
            r.ingress_bytes.to_string(),
            r.cost.map(|c| c.to_string()).unwrap_or_default(),
            cur.clone(),
        ]));
    }
    out
}

pub fn table_lines(report: &NetReport, color: bool) -> Vec<String> {
    let mut lines = vec![
        ui::header(&format!("Network egress by {}", report.group_by), color),
        String::new(),
    ];
    if report.items.is_empty() {
        lines.push("No network usage records found".to_string());
        return lines;
    }
    let cur = report.currency.clone().unwrap_or_default();
    let rows: Vec<Vec<Cell2>> = report
        .items
        .iter()
        .map(|r| {
            vec![
                (r.key.clone(), None),
                (gb(r.egress_bytes), None),
                (
                    r.cost
                        .map(|c| format!("{c:.2} {cur}"))
                        .unwrap_or_else(|| "-".into()),
                    None,
                ),
            ]
        })
        .collect();
    let heading = report.group_by.to_uppercase();
    lines.extend(ui::grid(
        &[heading.as_str(), "EGRESS GB", "COST"],
        &rows,
        color,
    ));
    lines.push(String::new());
    lines.push(ui::kv(
        "Total",
        &format!("{} GB egress", gb(report.totals.egress_bytes)),
        7,
        color,
    ));
    lines.push(ui::ansi(
        "Egress only, from byte counters (estimate, not an invoice). Ingress is never billed.",
        "2",
        color,
    ));
    lines
}

pub struct Options {
    pub tenant: Option<String>,
    pub from: Option<String>,
    pub to: Option<String>,
    pub group_by: GroupBy,
    pub output: UsageFormat,
}

pub async fn execute(client: &GryviaClient, opts: Options) -> Result<()> {
    if opts.group_by == GroupBy::Sku {
        return Err(anyhow!(
            "--group-by sku is a GPU grouping; with --network use tenant, peer-class, zone-class or day"
        ));
    }
    let lo = opts
        .from
        .as_deref()
        .map(|v| parse_bound(v, "from", false))
        .transpose()?;
    let hi = opts
        .to
        .as_deref()
        .map(|v| parse_bound(v, "to", true))
        .transpose()?;
    let api = super::capacity::gvk_api(
        client,
        "GryviaNetworkUsageRecord",
        "gryvianetworkusagerecords",
    );
    let list = api.list(&ListParams::default()).await.map_err(|e| {
        anyhow!(
            "Failed to list network usage records: {}",
            display::cluster_error_text(&e.to_string())
        )
    })?;
    let records: Vec<NetRecord> = list
        .items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .map(|v| record_from_json(&v))
        .collect();
    // Prices are optional: without a GryviaNetworkRate (or the CRD) the report is bytes only.
    let rates = super::capacity::gvk_api(client, "GryviaNetworkRate", "gryvianetworkrates")
        .list(&ListParams::default())
        .await
        .ok()
        .and_then(|l| {
            let mut items = l.items;
            items.sort_by(|a, b| a.metadata.name.cmp(&b.metadata.name));
            items.first().and_then(|o| serde_json::to_value(o).ok())
        })
        .map(|v| rates_from_json(&v));
    let used = clean(&records, opts.tenant.as_deref(), lo, hi);
    let report = aggregate(&used, opts.group_by, rates.as_ref());
    match opts.output {
        UsageFormat::Table => {
            for line in table_lines(&report, ui::color_enabled()) {
                println!("{line}");
            }
            Ok(())
        }
        UsageFormat::Json => crate::output::print_serialized("json", &report),
        UsageFormat::Yaml => crate::output::print_serialized("yaml", &report),
        UsageFormat::Csv => {
            for line in csv_lines(&report) {
                println!("{line}");
            }
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn rec(ns: &str, node: &str, peer: &str, zone: &str, egress: u64, hour: &str) -> NetRecord {
        record_from_json(&json!({
            "metadata": {"namespace": ns},
            "spec": {"tenant": ns.trim_start_matches("tenant-"), "node": node, "hour": hour,
                     "peerClass": peer, "zoneClass": zone, "egressBytes": egress, "ingressBytes": 7, "final": true}
        }))
    }

    fn rates() -> Rates {
        rates_from_json(
            &json!({"spec": {"sameZone": 0.01, "crossZone": 0.02, "internetEgress": 0.09}}),
        )
    }

    const H: &str = "2026-09-01T10:00:00Z";

    #[test]
    fn prices_only_egress_of_priced_zones() {
        let r = rates();
        assert_eq!(price("internet", 2e9, Some(&r)), Some(0.18));
        assert_eq!(price("unknown-zone", 2e9, Some(&r)), None);
        assert_eq!(price("internet", 2e9, None), None);
    }

    #[test]
    fn aggregates_by_tenant_and_zone_with_cost() {
        let recs = vec![
            rec("tenant-a", "n1", "external", "internet", 2_000_000_000, H),
            rec(
                "tenant-a",
                "n2",
                "same-tenant",
                "cross-zone",
                1_000_000_000,
                H,
            ),
            rec(
                "tenant-b",
                "n1",
                "unknown",
                "unknown-zone",
                5_000_000_000,
                H,
            ),
        ];
        let by_tenant = aggregate(
            &clean(&recs, None, None, None),
            GroupBy::Tenant,
            Some(&rates()),
        );
        assert_eq!(by_tenant.items[0].key, "b");
        assert_eq!(by_tenant.items[0].cost, Some(0.0));
        assert_eq!(by_tenant.items[1].cost, Some(0.2));
        assert_eq!(by_tenant.totals.egress_bytes, 8_000_000_000);
        assert_eq!(by_tenant.totals.ingress_bytes, 21);
        assert!(!by_tenant.open);
        let by_zone = aggregate(&recs, GroupBy::ZoneClass, None);
        assert_eq!(by_zone.items[0].key, "unknown-zone");
        assert_eq!(by_zone.items[0].cost, None);
    }

    #[test]
    fn duplicates_counted_once_and_other_sources_ignored() {
        let mut recs = vec![
            rec("tenant-a", "n1", "external", "internet", 1_000, H),
            rec("tenant-a", "n1", "external", "internet", 3_000, H),
            rec("tenant-a", "n2", "external", "internet", 1_000, H),
        ];
        let mut other = rec("tenant-a", "netra", "external", "internet", 9_999, H);
        other.source = "netra".into();
        recs.push(other);
        let used = clean(&recs, None, None, None);
        assert_eq!(
            aggregate(&used, GroupBy::Tenant, None).totals.egress_bytes,
            4_000
        );
    }

    #[test]
    fn hour_and_tenant_filters() {
        let recs = vec![
            rec(
                "tenant-a",
                "n1",
                "external",
                "internet",
                1,
                "2026-09-01T10:00:00Z",
            ),
            rec(
                "tenant-a",
                "n1",
                "external",
                "internet",
                2,
                "2026-09-02T00:00:00Z",
            ),
            rec(
                "tenant-b",
                "n1",
                "external",
                "internet",
                4,
                "2026-09-01T10:00:00Z",
            ),
        ];
        let lo = parse_bound("2026-09-01", "from", false).ok();
        let hi = parse_bound("2026-09-01", "to", true).ok();
        let used = clean(&recs, Some("a"), lo, hi);
        assert_eq!(used.len(), 1);
        assert_eq!(used[0].egress, 1.0);
    }

    #[test]
    fn csv_and_table_output() {
        let recs = vec![rec(
            "tenant-a",
            "n1",
            "external",
            "internet",
            2_000_000_000,
            H,
        )];
        let rep = aggregate(&recs, GroupBy::PeerClass, Some(&rates()));
        let csv = csv_lines(&rep);
        assert_eq!(csv[0], "peerClass,egress_bytes,ingress_bytes,cost,currency");
        assert_eq!(csv[1], "external,2000000000,7,0.18,USD");
        assert_eq!(csv.last().unwrap(), "total,2000000000,7,0.18,USD");
        let table = table_lines(&rep, false).join("\n");
        assert!(table.contains("estimate, not an invoice") && table.contains("2.000"));
        let json = serde_json::to_value(&rep).unwrap();
        assert_eq!(json["billing"], "egress-only");
        assert_eq!(json["totals"]["egressBytes"], 2_000_000_000u64);
    }
}
