// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia usage`: metered GPU usage aggregated from GryviaUsageRecord objects.
//!
//! Aggregation is client-side and follows the gateway (`routers/usage.py`): GPU hours and cost are summed,
//! jobs are counted once per (namespace, job), the currency is `MIXED` when records disagree, records are
//! filtered on `spec.start`, and a date-only `--to` covers that whole day. Costs are estimates from job
//! wall-clock run time multiplied by the SKU rate; nothing is invoiced.

use std::collections::{BTreeMap, BTreeSet};

use anyhow::{anyhow, bail, Result};
use chrono::{DateTime, Duration, NaiveDate, TimeZone, Utc};
use clap::ValueEnum;
use kube::api::ListParams;
use serde::Serialize;
use serde_json::Value;

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, ValueEnum)]
pub enum GroupBy {
    /// One row per tenant (default)
    #[default]
    Tenant,
    /// One row per SKU
    Sku,
    /// One row per UTC day
    Day,
    /// One row per peer class (only with --network)
    PeerClass,
    /// One row per zone class (only with --network)
    ZoneClass,
}

impl GroupBy {
    pub fn as_str(self) -> &'static str {
        match self {
            GroupBy::Tenant => "tenant",
            GroupBy::Sku => "sku",
            GroupBy::Day => "day",
            GroupBy::PeerClass => "peerClass",
            GroupBy::ZoneClass => "zoneClass",
        }
    }
}

/// `-o` for `gryvia usage`: the usual formats plus CSV.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, ValueEnum)]
pub enum UsageFormat {
    /// Human-readable table (default)
    #[default]
    Table,
    /// JSON, for scripts
    Json,
    /// YAML
    Yaml,
    /// CSV with a header row, for spreadsheets
    Csv,
}

/// A usage record as far as aggregation is concerned.
#[derive(Clone, Debug, PartialEq)]
pub struct Record {
    pub tenant: String,
    /// (namespace, job): what makes a job distinct.
    pub job_id: (String, String),
    pub sku: String,
    /// The raw `spec.gpuType` (empty when unset).
    pub gpu_type: String,
    /// `spec.final`: the job finished and the record will not change again.
    pub is_final: bool,
    pub start: Option<DateTime<Utc>>,
    pub gpu_hours: f64,
    pub cost: f64,
    pub currency: String,
    pub namespace: String,
}

fn ts(v: &Value, ptr: &str) -> Option<DateTime<Utc>> {
    DateTime::parse_from_rfc3339(v.pointer(ptr)?.as_str()?)
        .ok()
        .map(|d| d.with_timezone(&Utc))
}

fn num(v: &Value, ptr: &str) -> f64 {
    v.pointer(ptr).and_then(Value::as_f64).unwrap_or(0.0)
}

fn text(v: &Value, ptr: &str) -> String {
    v.pointer(ptr)
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string()
}

/// Read a record from its raw JSON. The tenant falls back to the `tenant-<name>` namespace and the start
/// time to `spec.end` and then the creation timestamp.
pub fn record_from_json(v: &Value) -> Record {
    let namespace = text(v, "/metadata/namespace");
    let tenant = match text(v, "/spec/tenant") {
        t if !t.is_empty() => t,
        _ => namespace
            .strip_prefix("tenant-")
            .unwrap_or(&namespace)
            .to_string(),
    };
    let job = match text(v, "/spec/job") {
        j if !j.is_empty() => j,
        _ => text(v, "/metadata/name"),
    };
    let sku = match text(v, "/spec/sku") {
        s if !s.is_empty() => s,
        _ => match text(v, "/spec/gpuType") {
            g if !g.is_empty() => g,
            _ => "unknown".to_string(),
        },
    };
    Record {
        tenant,
        job_id: (namespace.clone(), job),
        sku,
        gpu_type: text(v, "/spec/gpuType"),
        is_final: v
            .pointer("/spec/final")
            .and_then(Value::as_bool)
            .unwrap_or(false),
        start: ts(v, "/spec/start")
            .or_else(|| ts(v, "/spec/end"))
            .or_else(|| ts(v, "/metadata/creationTimestamp")),
        gpu_hours: num(v, "/spec/gpuHours"),
        cost: num(v, "/spec/cost"),
        currency: match text(v, "/spec/currency") {
            c if c.is_empty() => "USD".to_string(),
            c => c,
        },
        namespace,
    }
}

/// Parse `--from` / `--to`: a date (`2026-09-01`) or an RFC 3339 timestamp. A date-only value means the start
/// of that UTC day, or, when `end` is true, the start of the next day, so `--to 2026-09-30` covers all of
/// the 30th. The returned bound is exclusive for the upper end.
pub fn parse_bound(value: &str, name: &str, end: bool) -> Result<DateTime<Utc>> {
    if let Ok(d) = NaiveDate::parse_from_str(value, "%Y-%m-%d") {
        let start = Utc.from_utc_datetime(&d.and_hms_opt(0, 0, 0).unwrap());
        return Ok(if end {
            start + Duration::days(1)
        } else {
            start
        });
    }
    match DateTime::parse_from_rfc3339(value) {
        Ok(d) => Ok(d.with_timezone(&Utc)),
        Err(_) => {
            bail!("--{name} must be a date (YYYY-MM-DD) or an RFC 3339 timestamp, got '{value}'")
        }
    }
}

/// Keep records of `tenant` (matched on the tenant name or its `tenant-<name>` namespace) whose start lies
/// in `[lo, hi)`. A record without any timestamp is dropped when a bound is set.
pub fn filter_records(
    records: &[Record],
    tenant: Option<&str>,
    lo: Option<DateTime<Utc>>,
    hi: Option<DateTime<Utc>>,
) -> Vec<Record> {
    records
        .iter()
        .filter(|r| match tenant {
            Some(t) => r.tenant == t || r.namespace == format!("tenant-{t}"),
            None => true,
        })
        .filter(|r| {
            if lo.is_none() && hi.is_none() {
                return true;
            }
            match r.start {
                None => false,
                Some(s) => lo.is_none_or(|l| s >= l) && hi.is_none_or(|h| s < h),
            }
        })
        .cloned()
        .collect()
}

fn group_key(r: &Record, by: GroupBy) -> String {
    match by {
        GroupBy::Tenant => r.tenant.clone(),
        GroupBy::Sku => r.sku.clone(),
        GroupBy::Day => r
            .start
            .map(|s| s.date_naive().to_string())
            .unwrap_or_else(|| "unknown".to_string()),
        // Rejected in execute(): only network usage has these groupings.
        GroupBy::PeerClass | GroupBy::ZoneClass => r.tenant.clone(),
    }
}

fn currency_of(currencies: &BTreeSet<String>) -> String {
    match currencies.len() {
        0 => "USD".to_string(),
        1 => currencies.iter().next().unwrap().clone(),
        _ => "MIXED".to_string(),
    }
}

fn round4(x: f64) -> f64 {
    (x * 10_000.0).round() / 10_000.0
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct UsageRow {
    pub key: String,
    #[serde(rename = "gpuHours")]
    pub gpu_hours: f64,
    pub cost: f64,
    pub currency: String,
    pub jobs: usize,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct UsageTotals {
    #[serde(rename = "gpuHours")]
    pub gpu_hours: f64,
    pub cost: f64,
    pub currency: String,
    pub jobs: usize,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct UsageReport {
    #[serde(rename = "groupBy")]
    pub group_by: String,
    pub items: Vec<UsageRow>,
    pub totals: UsageTotals,
}

#[derive(Default)]
struct Acc {
    hours: f64,
    cost: f64,
    currencies: BTreeSet<String>,
    jobs: BTreeSet<(String, String)>,
}

/// Group records and total them. Tenant and SKU groups are ordered by cost (highest first), day groups by
/// date. Values are rounded to four decimals like the gateway's.
pub fn aggregate(records: &[Record], by: GroupBy) -> UsageReport {
    let mut groups: BTreeMap<String, Acc> = BTreeMap::new();
    let mut all = Acc::default();
    for r in records {
        let g = groups.entry(group_key(r, by)).or_default();
        for a in [g, &mut all] {
            a.hours += r.gpu_hours;
            a.cost += r.cost;
            a.currencies.insert(r.currency.clone());
            a.jobs.insert(r.job_id.clone());
        }
    }
    let mut items: Vec<UsageRow> = groups
        .into_iter()
        .map(|(key, a)| UsageRow {
            key,
            gpu_hours: round4(a.hours),
            cost: round4(a.cost),
            currency: currency_of(&a.currencies),
            jobs: a.jobs.len(),
        })
        .collect();
    if by != GroupBy::Day {
        // BTreeMap order is by key already; the stable sort keeps it for equal costs.
        items.sort_by(|a, b| b.cost.total_cmp(&a.cost));
    }
    UsageReport {
        group_by: by.as_str().to_string(),
        items,
        totals: UsageTotals {
            gpu_hours: round4(all.hours),
            cost: round4(all.cost),
            currency: currency_of(&all.currencies),
            jobs: all.jobs.len(),
        },
    }
}

/// Guard a CSV cell against spreadsheet formula injection, then quote it when needed.
pub fn csv_cell(value: &str) -> String {
    let guarded = if value.starts_with(['=', '+', '-', '@', '\t', '\r']) {
        format!("'{value}")
    } else {
        value.to_string()
    };
    if guarded.contains([',', '"', '\n', '\r']) {
        format!("\"{}\"", guarded.replace('"', "\"\""))
    } else {
        guarded
    }
}

/// The report as CSV: a header row, one row per group and a final `total` row.
pub fn csv_lines(report: &UsageReport) -> Vec<String> {
    let line = |cells: [String; 5]| {
        cells
            .iter()
            .map(|c| csv_cell(c))
            .collect::<Vec<_>>()
            .join(",")
    };
    let mut out = vec![line([
        report.group_by.clone(),
        "gpu_hours".into(),
        "cost".into(),
        "currency".into(),
        "jobs".into(),
    ])];
    for r in &report.items {
        out.push(line([
            r.key.clone(),
            r.gpu_hours.to_string(),
            r.cost.to_string(),
            r.currency.clone(),
            r.jobs.to_string(),
        ]));
    }
    let t = &report.totals;
    out.push(line([
        "total".into(),
        t.gpu_hours.to_string(),
        t.cost.to_string(),
        t.currency.clone(),
        t.jobs.to_string(),
    ]));
    out
}

/// The table report. Costs are estimates, and the report says so.
pub fn usage_lines(report: &UsageReport, color: bool) -> Vec<String> {
    let mut lines = vec![
        ui::header(&format!("GPU Usage by {}", report.group_by), color),
        String::new(),
    ];
    if report.items.is_empty() {
        lines.push("No usage records found".to_string());
        return lines;
    }
    let heading = report.group_by.to_uppercase();
    let rows: Vec<Vec<Cell2>> = report
        .items
        .iter()
        .map(|r| {
            vec![
                (r.key.clone(), None),
                (format!("{:.2}", r.gpu_hours), None),
                (format!("{:.2} {}", r.cost, r.currency), None),
                (r.jobs.to_string(), None),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &[heading.as_str(), "GPU HOURS", "COST", "JOBS"],
        &rows,
        color,
    ));
    let t = &report.totals;
    lines.push(String::new());
    lines.push(ui::kv(
        "Total",
        &format!(
            "{:.2} GPU hours, {:.2} {}, {} jobs",
            t.gpu_hours, t.cost, t.currency, t.jobs
        ),
        7,
        color,
    ));
    lines.push(ui::ansi(
        "Costs are estimates from job run time, not invoices.",
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
    if matches!(opts.group_by, GroupBy::PeerClass | GroupBy::ZoneClass) {
        bail!("--group-by peer-class and zone-class need --network");
    }
    // Validate the dates before touching the cluster.
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
    let api = super::capacity::gvk_api(client, "GryviaUsageRecord", "gryviausagerecords");
    let list = api.list(&ListParams::default()).await.map_err(|e| {
        anyhow!(
            "Failed to list usage records: {}",
            display::cluster_error_text(&e.to_string())
        )
    })?;
    let records: Vec<Record> = list
        .items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .map(|v| record_from_json(&v))
        .collect();
    let filtered = filter_records(&records, opts.tenant.as_deref(), lo, hi);
    let report = aggregate(&filtered, opts.group_by);
    match opts.output {
        UsageFormat::Table => {
            for line in usage_lines(&report, ui::color_enabled()) {
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

    fn rec(
        ns: &str,
        job: &str,
        sku: &str,
        start: &str,
        hours: f64,
        cost: f64,
        cur: &str,
    ) -> Record {
        record_from_json(&json!({
            "metadata": {"namespace": ns, "name": format!("{job}-rec")},
            "spec": {"tenant": ns.trim_start_matches("tenant-"), "job": job, "sku": sku, "start": start,
                     "gpuHours": hours, "cost": cost, "currency": cur}
        }))
    }

    fn fixture() -> Vec<Record> {
        vec![
            rec(
                "tenant-acme",
                "a",
                "h100",
                "2026-09-01T10:00:00Z",
                8.0,
                100.0,
                "USD",
            ),
            rec(
                "tenant-acme",
                "b",
                "a100",
                "2026-09-02T23:30:00Z",
                2.0,
                4.0,
                "USD",
            ),
            rec(
                "tenant-beta",
                "a",
                "h100",
                "2026-09-02T01:00:00Z",
                1.5,
                30.0,
                "USD",
            ),
        ]
    }

    #[test]
    fn groups_by_tenant_sorted_by_cost() {
        let r = aggregate(&fixture(), GroupBy::Tenant);
        assert_eq!(r.group_by, "tenant");
        let keys: Vec<_> = r.items.iter().map(|i| i.key.as_str()).collect();
        assert_eq!(keys, ["acme", "beta"]);
        assert_eq!(r.items[0].gpu_hours, 10.0);
        assert_eq!(r.items[0].cost, 104.0);
        assert_eq!(r.items[0].jobs, 2);
        // job "a" exists in both namespaces: distinct jobs are per (namespace, job)
        assert_eq!(r.totals.jobs, 3);
        assert_eq!(r.totals.gpu_hours, 11.5);
        assert_eq!(r.totals.cost, 134.0);
        assert_eq!(r.totals.currency, "USD");
    }

    #[test]
    fn groups_by_sku_and_day() {
        let r = aggregate(&fixture(), GroupBy::Sku);
        assert_eq!(r.items[0].key, "h100");
        assert_eq!(r.items[0].cost, 130.0);
        assert_eq!(r.items[1].key, "a100");
        let r = aggregate(&fixture(), GroupBy::Day);
        let keys: Vec<_> = r.items.iter().map(|i| i.key.as_str()).collect();
        assert_eq!(keys, ["2026-09-01", "2026-09-02"]);
        assert_eq!(r.items[1].jobs, 2);
    }

    #[test]
    fn mixed_currencies() {
        let mut recs = fixture();
        recs.push(rec(
            "tenant-beta",
            "c",
            "h100",
            "2026-09-03T00:00:00Z",
            1.0,
            5.0,
            "EUR",
        ));
        let r = aggregate(&recs, GroupBy::Tenant);
        assert_eq!(r.totals.currency, "MIXED");
        assert_eq!(
            r.items.iter().find(|i| i.key == "beta").unwrap().currency,
            "MIXED"
        );
        assert_eq!(
            r.items.iter().find(|i| i.key == "acme").unwrap().currency,
            "USD"
        );
    }

    #[test]
    fn empty_aggregate() {
        let r = aggregate(&[], GroupBy::Tenant);
        assert!(r.items.is_empty());
        assert_eq!(r.totals.jobs, 0);
        assert_eq!(r.totals.cost, 0.0);
        assert!(usage_lines(&r, false)
            .join("\n")
            .contains("No usage records found"));
        assert_eq!(csv_lines(&r).len(), 2); // header and total
    }

    #[test]
    fn date_only_to_covers_the_whole_day() {
        let lo = parse_bound("2026-09-02", "from", false).unwrap();
        let hi = parse_bound("2026-09-02", "to", true).unwrap();
        let kept = filter_records(&fixture(), None, Some(lo), Some(hi));
        let jobs: Vec<_> = kept.iter().map(|r| r.job_id.1.as_str()).collect();
        assert_eq!(jobs, ["b", "a"]); // 23:30 on the 2nd is included, the 1st is not
        let only_from = filter_records(&fixture(), None, Some(lo), None);
        assert_eq!(only_from.len(), 2);
        let ts_to = parse_bound("2026-09-02T01:00:00Z", "to", true).unwrap();
        assert_eq!(filter_records(&fixture(), None, None, Some(ts_to)).len(), 1);
    }

    #[test]
    fn bad_dates_are_rejected() {
        assert!(parse_bound("yesterday", "from", false).is_err());
        assert!(parse_bound("2026-13-01", "to", true).is_err());
    }

    #[test]
    fn tenant_filter_matches_name_or_namespace() {
        let mut recs = fixture();
        recs.push(record_from_json(&json!({
            "metadata": {"namespace": "tenant-gamma", "name": "x"},
            "spec": {"job": "x", "gpuHours": 1, "cost": 1}
        })));
        assert_eq!(filter_records(&recs, Some("acme"), None, None).len(), 2);
        assert_eq!(filter_records(&recs, Some("gamma"), None, None).len(), 1);
        assert_eq!(filter_records(&recs, Some("nobody"), None, None).len(), 0);
    }

    #[test]
    fn records_without_a_timestamp_are_dropped_by_date_filters_only() {
        let r = record_from_json(
            &json!({"metadata": {"namespace": "tenant-a", "name": "x"}, "spec": {"job": "x"}}),
        );
        let lo = parse_bound("2026-01-01", "from", false).unwrap();
        assert!(filter_records(std::slice::from_ref(&r), None, Some(lo), None).is_empty());
        assert_eq!(filter_records(&[r], None, None, None).len(), 1);
    }

    #[test]
    fn csv_cells_are_injection_safe_and_quoted() {
        assert_eq!(csv_cell("acme"), "acme");
        for lead in ["=SUM(A1)", "+1", "-1", "@cmd"] {
            assert_eq!(csv_cell(lead), format!("'{lead}"));
        }
        assert_eq!(csv_cell("a,b"), "\"a,b\"");
        assert_eq!(csv_cell("say \"hi\""), "\"say \"\"hi\"\"\"");
        assert_eq!(csv_cell("=a,b"), "\"'=a,b\"");
    }

    #[test]
    fn csv_report() {
        let r = aggregate(&fixture(), GroupBy::Tenant);
        assert_eq!(
            csv_lines(&r),
            [
                "tenant,gpu_hours,cost,currency,jobs",
                "acme,10,104,USD,2",
                "beta,1.5,30,USD,1",
                "total,11.5,134,USD,3",
            ]
        );
        let evil = aggregate(
            &[rec(
                "tenant-x",
                "j",
                "=HYPERLINK(\"x\")",
                "2026-09-01T00:00:00Z",
                1.0,
                1.0,
                "USD",
            )],
            GroupBy::Sku,
        );
        assert!(csv_lines(&evil)[1].starts_with("\"'=HYPERLINK"));
    }

    #[test]
    fn table_report_says_costs_are_estimates() {
        let text = usage_lines(&aggregate(&fixture(), GroupBy::Tenant), false).join("\n");
        let expected = "\
━━━ GPU Usage by tenant ━━━

TENANT  GPU HOURS  COST        JOBS
acme    10.00      104.00 USD  2
beta    1.50       30.00 USD   1

Total  11.50 GPU hours, 134.00 USD, 3 jobs
Costs are estimates from job run time, not invoices.";
        assert_eq!(text, expected);
    }

    #[test]
    fn json_report_shape_matches_the_gateway() {
        let v = serde_json::to_value(aggregate(&fixture(), GroupBy::Tenant)).unwrap();
        assert_eq!(v["groupBy"], "tenant");
        assert_eq!(v["items"][0]["gpuHours"], 10.0);
        assert_eq!(v["totals"]["jobs"], 3);
    }
}
