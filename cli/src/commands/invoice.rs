// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia invoice`: monthly invoice estimates built client-side from GryviaUsageRecord objects.
//!
//! The rules match the gateway: records are bucketed by `spec.start` in UTC, one invoice per tenant with
//! usage, one line per (SKU, GPU type), amounts rounded to 2 decimals, GPU hours to 4 and the effective
//! rate (amount / GPU hours) to 4. The currency is `MIXED` when records disagree. `open` is true while any
//! included record is not final. Nothing is billed: the status is always `estimate`.

use std::collections::{BTreeMap, BTreeSet};

use anyhow::{anyhow, Result};
use chrono::{DateTime, Datelike, NaiveDate, SecondsFormat, Utc};
use kube::api::ListParams;
use serde::Serialize;
use serde_json::Value;

use super::usage::{csv_cell, record_from_json, Record, UsageFormat};
use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

const NOTE: &str = "Estimate from job run time; not a tax invoice.";
const NOTE_OPEN: &str =
    "Estimate from job run time; not a tax invoice. Some jobs are still running, so amounts may change.";

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Period {
    pub from: String,
    pub to: String,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Line {
    pub sku: String,
    #[serde(rename = "gpuType")]
    pub gpu_type: String,
    #[serde(rename = "gpuHours")]
    pub gpu_hours: f64,
    pub rate: f64,
    pub amount: f64,
    pub jobs: usize,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Invoice {
    pub number: String,
    pub tenant: String,
    pub period: Period,
    pub currency: String,
    pub status: String,
    #[serde(rename = "generatedAt")]
    pub generated_at: String,
    pub lines: Vec<Line>,
    pub subtotal: f64,
    pub jobs: usize,
    pub open: bool,
    pub note: String,
}

/// Parse `--month` (`YYYY-MM`) into (year, month). Used as a clap value parser, so a bad value is a usage error.
pub fn parse_month(value: &str) -> Result<(i32, u32), String> {
    let bad = || format!("month must be YYYY-MM (for example 2026-09), got '{value}'");
    let (y, m) = value.split_once('-').ok_or_else(bad)?;
    if y.len() != 4 || m.len() != 2 || !y.chars().chain(m.chars()).all(|c| c.is_ascii_digit()) {
        return Err(bad());
    }
    let (y, m): (i32, u32) = (y.parse().map_err(|_| bad())?, m.parse().map_err(|_| bad())?);
    NaiveDate::from_ymd_opt(y, m, 1).ok_or_else(bad)?;
    Ok((y, m))
}

/// The UTC month containing `now`.
pub fn month_of(now: DateTime<Utc>) -> (i32, u32) {
    (now.year(), now.month())
}

fn month_bounds(year: i32, month: u32) -> (NaiveDate, NaiveDate) {
    let first = NaiveDate::from_ymd_opt(year, month, 1).expect("validated month");
    let next = if month == 12 {
        NaiveDate::from_ymd_opt(year + 1, 1, 1)
    } else {
        NaiveDate::from_ymd_opt(year, month + 1, 1)
    }
    .expect("valid next month");
    (first, next.pred_opt().expect("has a previous day"))
}

fn round2(x: f64) -> f64 {
    (x * 100.0).round() / 100.0
}

fn round4(x: f64) -> f64 {
    (x * 10_000.0).round() / 10_000.0
}

#[derive(Default)]
struct Acc {
    hours: f64,
    cost: f64,
    jobs: BTreeSet<(String, String)>,
}

fn currency_of(currencies: &BTreeSet<String>) -> String {
    match currencies.len() {
        0 => "USD".to_string(),
        1 => currencies.iter().next().unwrap().clone(),
        _ => "MIXED".to_string(),
    }
}

/// Build the invoices for one UTC month. Only records whose start lies in the month count; `tenant`
/// restricts to one tenant (name or `tenant-<name>` namespace). Tenants are ordered by name and lines by
/// amount (highest first).
pub fn build_invoices(
    records: &[Record],
    tenant: Option<&str>,
    year: i32,
    month: u32,
    generated_at: DateTime<Utc>,
) -> Vec<Invoice> {
    let (first, last) = month_bounds(year, month);
    let mut per_tenant: BTreeMap<&str, Vec<&Record>> = BTreeMap::new();
    for r in records {
        if let Some(t) = tenant {
            if r.tenant != t && r.namespace != format!("tenant-{t}") {
                continue;
            }
        }
        match r.start {
            Some(s) if s.year() == year && s.month() == month => {
                per_tenant.entry(r.tenant.as_str()).or_default().push(r)
            }
            _ => {}
        }
    }
    per_tenant
        .into_iter()
        .map(|(name, recs)| {
            let mut lines: BTreeMap<(String, String), Acc> = BTreeMap::new();
            let mut all_jobs = BTreeSet::new();
            let mut currencies = BTreeSet::new();
            let mut total = 0.0;
            let mut open = false;
            for r in recs {
                let sku = if r.sku.is_empty() {
                    &r.gpu_type
                } else {
                    &r.sku
                };
                let a = lines.entry((sku.clone(), r.gpu_type.clone())).or_default();
                a.hours += r.gpu_hours;
                a.cost += r.cost;
                a.jobs.insert(r.job_id.clone());
                all_jobs.insert(r.job_id.clone());
                currencies.insert(r.currency.clone());
                total += r.cost;
                open |= !r.is_final;
            }
            let mut lines: Vec<Line> = lines
                .into_iter()
                .map(|((sku, gpu_type), a)| {
                    let amount = round2(a.cost);
                    let hours = round4(a.hours);
                    Line {
                        sku,
                        gpu_type,
                        gpu_hours: hours,
                        rate: if hours > 0.0 {
                            round4(amount / hours)
                        } else {
                            0.0
                        },
                        amount,
                        jobs: a.jobs.len(),
                    }
                })
                .collect();
            lines.sort_by(|a, b| b.amount.total_cmp(&a.amount));
            Invoice {
                number: format!("INV-{name}-{year:04}{month:02}"),
                tenant: name.to_string(),
                period: Period {
                    from: first.to_string(),
                    to: last.to_string(),
                },
                currency: currency_of(&currencies),
                status: "estimate".to_string(),
                generated_at: generated_at.to_rfc3339_opts(SecondsFormat::Secs, true),
                lines,
                subtotal: round2(total),
                jobs: all_jobs.len(),
                open,
                note: if open { NOTE_OPEN } else { NOTE }.to_string(),
            }
        })
        .collect()
}

/// CSV for all invoices: one header, one row per line and a `TOTAL` row per invoice.
pub fn csv_lines(invoices: &[Invoice]) -> Vec<String> {
    let row = |cells: Vec<String>| {
        cells
            .iter()
            .map(|c| csv_cell(c))
            .collect::<Vec<_>>()
            .join(",")
    };
    let mut out = vec![
        "invoice,tenant,period_from,period_to,sku,gpuType,jobs,gpuHours,rate,amount,currency"
            .to_string(),
    ];
    for inv in invoices {
        for l in &inv.lines {
            out.push(row(vec![
                inv.number.clone(),
                inv.tenant.clone(),
                inv.period.from.clone(),
                inv.period.to.clone(),
                l.sku.clone(),
                l.gpu_type.clone(),
                l.jobs.to_string(),
                l.gpu_hours.to_string(),
                l.rate.to_string(),
                l.amount.to_string(),
                inv.currency.clone(),
            ]));
        }
        let hours: f64 = inv.lines.iter().map(|l| l.gpu_hours).sum();
        out.push(row(vec![
            inv.number.clone(),
            inv.tenant.clone(),
            inv.period.from.clone(),
            inv.period.to.clone(),
            "TOTAL".into(),
            String::new(),
            inv.jobs.to_string(),
            round4(hours).to_string(),
            String::new(),
            inv.subtotal.to_string(),
            inv.currency.clone(),
        ]));
    }
    out
}

/// The table output: one block per invoice.
pub fn invoice_lines(invoices: &[Invoice], color: bool) -> Vec<String> {
    let mut out = Vec::new();
    for (i, inv) in invoices.iter().enumerate() {
        if i > 0 {
            out.push(String::new());
        }
        out.push(ui::header(&format!("Invoice {}", inv.number), color));
        out.push(String::new());
        out.push(ui::kv("Tenant", &inv.tenant, 8, color));
        out.push(ui::kv(
            "Period",
            &format!("{} to {}", inv.period.from, inv.period.to),
            8,
            color,
        ));
        let status = if inv.open {
            format!(
                "{} {} {}",
                inv.status,
                Marker::Warn.paint_with(Marker::Warn.glyph(), color),
                "open"
            )
        } else {
            inv.status.clone()
        };
        out.push(ui::kv("Status", &status, 8, color));
        out.push(String::new());
        let rows: Vec<Vec<Cell2>> = inv
            .lines
            .iter()
            .map(|l| {
                vec![
                    (l.sku.clone(), None),
                    (l.gpu_type.clone(), None),
                    (l.jobs.to_string(), None),
                    (format!("{:.2}", l.gpu_hours), None),
                    (format!("{:.4}", l.rate), None),
                    (format!("{:.2} {}", l.amount, inv.currency), None),
                ]
            })
            .collect();
        out.extend(ui::grid(
            &["SKU", "GPU TYPE", "JOBS", "GPU HOURS", "RATE/H", "AMOUNT"],
            &rows,
            color,
        ));
        out.push(String::new());
        out.push(ui::kv(
            "Total",
            &format!("{:.2} {} ({} jobs)", inv.subtotal, inv.currency, inv.jobs),
            7,
            color,
        ));
        out.push(ui::ansi(&inv.note, "2", color));
    }
    out
}

pub struct Options {
    pub tenant: Option<String>,
    pub month: Option<(i32, u32)>,
    pub output: UsageFormat,
}

pub async fn execute(client: &GryviaClient, opts: Options) -> Result<()> {
    let now = Utc::now();
    let (year, month) = opts.month.unwrap_or_else(|| month_of(now));
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
        .map(|v: Value| record_from_json(&v))
        .collect();
    let invoices = build_invoices(&records, opts.tenant.as_deref(), year, month, now);
    match opts.output {
        UsageFormat::Json => crate::output::print_serialized("json", &invoices),
        UsageFormat::Yaml => crate::output::print_serialized("yaml", &invoices),
        UsageFormat::Csv => {
            for line in csv_lines(&invoices) {
                println!("{line}");
            }
            Ok(())
        }
        UsageFormat::Table => {
            if invoices.is_empty() {
                println!("No usage found for {year:04}-{month:02}");
            }
            for line in invoice_lines(&invoices, ui::color_enabled()) {
                println!("{line}");
            }
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;
    use serde_json::json;

    #[allow(clippy::too_many_arguments)]
    fn rec(
        ns: &str,
        job: &str,
        sku: &str,
        start: &str,
        hours: f64,
        cost: f64,
        cur: &str,
        fin: bool,
    ) -> Record {
        record_from_json(&json!({
            "metadata": {"namespace": ns, "name": format!("{job}-rec")},
            "spec": {"tenant": ns.trim_start_matches("tenant-"), "job": job, "sku": sku,
                     "gpuType": "H100", "start": start, "gpuHours": hours, "cost": cost,
                     "currency": cur, "final": fin}
        }))
    }

    fn now() -> DateTime<Utc> {
        Utc.with_ymd_and_hms(2026, 10, 1, 8, 0, 0).unwrap()
    }

    fn fixture() -> Vec<Record> {
        vec![
            rec(
                "tenant-acme",
                "a",
                "h100-8x",
                "2026-09-01T00:00:00Z",
                8.0,
                100.0,
                "USD",
                true,
            ),
            rec(
                "tenant-acme",
                "b",
                "h100-8x",
                "2026-09-30T23:59:59Z",
                1.0,
                12.5,
                "USD",
                true,
            ),
            rec(
                "tenant-acme",
                "c",
                "a100",
                "2026-09-10T10:00:00Z",
                2.0,
                4.0,
                "USD",
                true,
            ),
            // outside September, on both sides
            rec(
                "tenant-acme",
                "d",
                "a100",
                "2026-08-31T23:59:59Z",
                9.0,
                9.0,
                "USD",
                true,
            ),
            rec(
                "tenant-acme",
                "e",
                "a100",
                "2026-10-01T00:00:00Z",
                9.0,
                9.0,
                "USD",
                true,
            ),
            rec(
                "tenant-beta",
                "a",
                "h100-8x",
                "2026-09-05T00:00:00Z",
                3.0,
                30.0,
                "USD",
                true,
            ),
        ]
    }

    #[test]
    fn month_parsing() {
        assert_eq!(parse_month("2026-09"), Ok((2026, 9)));
        assert_eq!(parse_month("2026-12"), Ok((2026, 12)));
        for bad in [
            "2026-13",
            "2026-00",
            "2026-9",
            "26-09",
            "2026/09",
            "2026-09-01",
            "",
            "abcd-ef",
        ] {
            assert!(parse_month(bad).is_err(), "{bad}");
        }
        assert_eq!(month_of(now()), (2026, 10));
    }

    #[test]
    fn month_boundaries_and_period() {
        let inv = build_invoices(&fixture(), Some("acme"), 2026, 9, now());
        assert_eq!(inv.len(), 1);
        let i = &inv[0];
        assert_eq!(i.number, "INV-acme-202609");
        assert_eq!(
            i.period,
            Period {
                from: "2026-09-01".into(),
                to: "2026-09-30".into()
            }
        );
        assert_eq!(i.jobs, 3); // d and e fall outside the month
        assert_eq!(i.subtotal, 116.5);
        assert_eq!(i.generated_at, "2026-10-01T08:00:00Z");
        assert_eq!(i.status, "estimate");
        let feb = month_bounds(2028, 2);
        assert_eq!(feb.1.to_string(), "2028-02-29");
        assert_eq!(month_bounds(2026, 12).1.to_string(), "2026-12-31");
        let dec = build_invoices(
            &[rec(
                "tenant-a",
                "x",
                "s",
                "2026-12-31T23:00:00Z",
                1.0,
                1.0,
                "USD",
                true,
            )],
            None,
            2026,
            12,
            now(),
        );
        assert_eq!(dec[0].number, "INV-a-202612");
    }

    #[test]
    fn one_line_per_sku_ordered_by_amount() {
        let inv = build_invoices(&fixture(), None, 2026, 9, now());
        assert_eq!(
            inv.iter().map(|i| i.tenant.as_str()).collect::<Vec<_>>(),
            ["acme", "beta"]
        );
        let lines = &inv[0].lines;
        assert_eq!(lines.len(), 2);
        assert_eq!(lines[0].sku, "h100-8x");
        assert_eq!(lines[0].gpu_type, "H100");
        assert_eq!(lines[0].gpu_hours, 9.0);
        assert_eq!(lines[0].amount, 112.5);
        assert_eq!(lines[0].rate, 12.5);
        assert_eq!(lines[0].jobs, 2);
        assert_eq!(lines[1].sku, "a100");
        assert_eq!(inv[1].subtotal, 30.0);
    }

    #[test]
    fn sku_falls_back_to_gpu_type_and_rounding() {
        let r = record_from_json(&json!({
            "metadata": {"namespace": "tenant-z", "name": "n"},
            "spec": {"job": "j", "gpuType": "L4", "start": "2026-09-02T00:00:00Z",
                     "gpuHours": 0.40001, "cost": 1.004, "final": true}
        }));
        let r2 = Record {
            job_id: ("tenant-z".into(), "k".into()),
            gpu_hours: 0.40001,
            cost: 1.0,
            ..r.clone()
        };
        let inv = build_invoices(&[r, r2], None, 2026, 9, now());
        let l = &inv[0].lines[0];
        assert_eq!(l.sku, "L4");
        assert_eq!(l.gpu_hours, 0.8);
        assert_eq!(l.amount, 2.0); // 2.004 rounds to 2 decimals
        assert_eq!(l.rate, 2.5); // 2.0 / 0.8
        assert_eq!(inv[0].subtotal, 2.0);
        // zero hours never divides
        let z = record_from_json(&json!({
            "metadata": {"namespace": "tenant-z", "name": "n"},
            "spec": {"job": "j", "sku": "s", "start": "2026-09-02T00:00:00Z", "cost": 1, "final": true}
        }));
        assert_eq!(
            build_invoices(&[z], None, 2026, 9, now())[0].lines[0].rate,
            0.0
        );
    }

    #[test]
    fn mixed_currency() {
        let mut recs = fixture();
        recs.push(rec(
            "tenant-beta",
            "z",
            "h100-8x",
            "2026-09-06T00:00:00Z",
            1.0,
            5.0,
            "EUR",
            true,
        ));
        let inv = build_invoices(&recs, None, 2026, 9, now());
        assert_eq!(inv[0].currency, "USD");
        assert_eq!(inv[1].currency, "MIXED");
    }

    #[test]
    fn open_flag_and_note() {
        let mut recs = fixture();
        assert!(build_invoices(&recs, Some("acme"), 2026, 9, now())
            .iter()
            .all(|i| !i.open && i.note == NOTE));
        recs.push(rec(
            "tenant-acme",
            "run",
            "a100",
            "2026-09-29T00:00:00Z",
            1.0,
            2.0,
            "USD",
            false,
        ));
        let inv = build_invoices(&recs, None, 2026, 9, now());
        assert!(inv[0].open);
        assert!(!inv[1].open);
        assert!(inv[0].note.contains("still running"));
        // a running record outside the month does not open the invoice
        let out = vec![rec(
            "tenant-a",
            "r",
            "s",
            "2026-08-01T00:00:00Z",
            1.0,
            1.0,
            "USD",
            false,
        )];
        assert!(build_invoices(&out, None, 2026, 9, now()).is_empty());
    }

    #[test]
    fn tenant_filter_and_empty() {
        assert_eq!(
            build_invoices(&fixture(), Some("beta"), 2026, 9, now()).len(),
            1
        );
        assert!(build_invoices(&fixture(), Some("nobody"), 2026, 9, now()).is_empty());
        assert!(build_invoices(&fixture(), None, 2025, 1, now()).is_empty());
        assert!(build_invoices(&[], None, 2026, 9, now()).is_empty());
        assert_eq!(csv_lines(&[]).len(), 1); // header only
        assert!(invoice_lines(&[], false).is_empty());
    }

    #[test]
    fn csv_output_and_hardening() {
        let inv = build_invoices(&fixture(), Some("beta"), 2026, 9, now());
        assert_eq!(
            csv_lines(&inv),
            [
                "invoice,tenant,period_from,period_to,sku,gpuType,jobs,gpuHours,rate,amount,currency",
                "INV-beta-202609,beta,2026-09-01,2026-09-30,h100-8x,H100,1,3,10,30,USD",
                "INV-beta-202609,beta,2026-09-01,2026-09-30,TOTAL,,1,3,,30,USD",
            ]
        );
        let evil = build_invoices(
            &[rec(
                "tenant-x",
                "j",
                "=HYPERLINK(\"x\")",
                "2026-09-01T00:00:00Z",
                1.0,
                1.0,
                "USD",
                true,
            )],
            None,
            2026,
            9,
            now(),
        );
        assert!(csv_lines(&evil)[1].contains(",\"'=HYPERLINK("));
    }

    #[test]
    fn table_output() {
        let mut recs = fixture();
        recs.push(rec(
            "tenant-beta",
            "run",
            "h100-8x",
            "2026-09-06T00:00:00Z",
            1.0,
            10.0,
            "USD",
            false,
        ));
        let inv = build_invoices(&recs, Some("beta"), 2026, 9, now());
        let text = invoice_lines(&inv, false).join("\n");
        let expected = "\
━━━ Invoice INV-beta-202609 ━━━

Tenant  beta
Period  2026-09-01 to 2026-09-30
Status  estimate ! open

SKU      GPU TYPE  JOBS  GPU HOURS  RATE/H   AMOUNT
h100-8x  H100      2     4.00       10.0000  40.00 USD

Total  40.00 USD (2 jobs)
Estimate from job run time; not a tax invoice. Some jobs are still running, so amounts may change.";
        assert_eq!(text, expected);
    }

    #[test]
    fn json_shape() {
        let v =
            serde_json::to_value(build_invoices(&fixture(), Some("acme"), 2026, 9, now())).unwrap();
        assert!(v.is_array());
        assert_eq!(v[0]["number"], "INV-acme-202609");
        assert_eq!(v[0]["period"]["to"], "2026-09-30");
        assert_eq!(v[0]["lines"][0]["gpuHours"], 9.0);
        assert_eq!(v[0]["generatedAt"], "2026-10-01T08:00:00Z");
        assert_eq!(v[0]["open"], false);
    }
}
