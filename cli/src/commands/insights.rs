// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia insights`: queue positions, wait times, failure causes, GPU right-sizing and quota use, read from the
//! API gateway's `/api/queue`, `/api/queue/stats`, `/api/failures`, `/api/rightsizing` and `/api/quota-usage`.

use anyhow::{anyhow, Result};
use serde_json::Value;

use crate::gateway::GatewayClient;
use crate::output;
use crate::ui::{self, Cell2, Marker};

fn text(v: &Value, key: &str) -> String {
    match v.get(key) {
        Some(Value::String(s)) => s.clone(),
        Some(Value::Number(n)) => n.to_string(),
        Some(Value::Bool(b)) => b.to_string(),
        _ => "-".to_string(),
    }
}

fn num(v: &Value, key: &str) -> Option<f64> {
    v.get(key).and_then(Value::as_f64)
}

fn pct(v: &Value, key: &str) -> String {
    num(v, key).map_or("-".to_string(), |x| format!("{x:.1}%"))
}

/// "95s", "12m", "3h20m": a duration in seconds for a table cell.
pub fn fmt_secs(secs: Option<f64>) -> String {
    match secs {
        None => "-".to_string(),
        Some(s) if s < 120.0 => format!("{}s", s.round() as i64),
        Some(s) if s < 7200.0 => format!("{}m", (s / 60.0).round() as i64),
        Some(s) => {
            let m = (s / 60.0).round() as i64;
            format!("{}h{:02}m", m / 60, m % 60)
        }
    }
}

/// Percent-encode a query value (letters, digits and `-_.~` pass through).
pub fn enc(v: &str) -> String {
    v.bytes()
        .map(|b| match b {
            b'a'..=b'z' | b'A'..=b'Z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                (b as char).to_string()
            }
            _ => format!("%{b:02X}"),
        })
        .collect()
}

fn items(body: &Value, key: &str) -> Vec<Value> {
    body.get(key)
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default()
}

fn c(s: impl Into<String>) -> Cell2 {
    (s.into(), None)
}

pub fn queue_rows(body: &Value) -> Vec<Vec<Cell2>> {
    items(body, "items")
        .iter()
        .map(|i| {
            vec![
                c(text(i, "position")),
                c(text(i, "name")),
                c(text(i, "tenant")),
                c(text(i, "queue")),
                c(text(i, "gpus")),
                c(text(i, "priority")),
                c(fmt_secs(num(i, "waitingSeconds"))),
                c(text(i, "reason")),
            ]
        })
        .collect()
}

pub fn stats_rows(body: &Value) -> Vec<Vec<Cell2>> {
    items(body, "byTenant")
        .iter()
        .map(|i| {
            vec![
                c(text(i, "tenant")),
                c(text(i, "started")),
                c(fmt_secs(num(i, "p50Seconds"))),
                c(fmt_secs(num(i, "p90Seconds"))),
                c(fmt_secs(num(i, "maxSeconds"))),
            ]
        })
        .collect()
}

pub fn failure_rows(body: &Value) -> Vec<Vec<Cell2>> {
    items(body, "categories")
        .iter()
        .map(|i| {
            let transient = i.get("transient").and_then(Value::as_bool) == Some(true);
            vec![
                c(text(i, "category")),
                c(text(i, "jobs")),
                c(pct(
                    &serde_json::json!({"x": num(i, "share").map(|s| s * 100.0)}),
                    "x",
                )),
                c(text(i, "gpus")),
                (
                    if transient {
                        "retry may help"
                    } else {
                        "will repeat"
                    }
                    .to_string(),
                    Some(if transient {
                        Marker::Warn
                    } else {
                        Marker::Error
                    }),
                ),
            ]
        })
        .collect()
}

pub fn rightsizing_rows(body: &Value) -> Vec<Vec<Cell2>> {
    items(body, "items")
        .iter()
        .map(|i| {
            let rec = i.get("recommendation").cloned().unwrap_or(Value::Null);
            let advice = match num(&rec, "gpuShare") {
                Some(s) => format!("share {s}"),
                None => format!("{} GPUs", text(&rec, "gpus")),
            };
            vec![
                c(text(i, "name")),
                c(text(i, "tenant")),
                c(text(i, "gpus")),
                c(pct(i, "utilizationPct")),
                c(advice),
                c(num(i, "savedGpuHours").map_or("-".to_string(), |h| format!("{h:.1}"))),
            ]
        })
        .collect()
}

pub fn quota_rows(body: &Value) -> Vec<Vec<Cell2>> {
    items(body, "items")
        .iter()
        .map(|i| {
            let blocked = i.get("blocked").and_then(Value::as_bool) == Some(true);
            let borrowing = i.get("borrowing").and_then(Value::as_bool) == Some(true);
            let state = if blocked {
                ("blocked", Some(Marker::Error))
            } else if borrowing {
                ("borrowing", Some(Marker::Warn))
            } else {
                ("ok", Some(Marker::Ok))
            };
            vec![
                c(text(i, "tenant")),
                c(text(i, "quotaGpus")),
                c(text(i, "gpusInUse")),
                c(text(i, "gpusWaiting")),
                c(pct(i, "utilizationPct")),
                (state.0.to_string(), state.1),
            ]
        })
        .collect()
}

fn show(title: &str, headers: &[&str], rows: &[Vec<Cell2>], footer: &str) {
    let color = ui::color_enabled();
    println!("{}", ui::header(title, color));
    println!();
    if rows.is_empty() {
        println!("Nothing to report.");
    } else {
        for line in ui::grid(headers, rows, color) {
            println!("{line}");
        }
    }
    println!();
    println!("{footer}");
}

pub async fn run(gateway: &GatewayClient, kind: &str, path: &str, output: &str) -> Result<()> {
    let v = gateway
        .get_json(path)
        .await
        .map_err(|e| anyhow!("insights {kind}: {e}"))?;
    if output != "table" {
        return output::print_serialized(output, &v);
    }
    match kind {
        "queue" => show(
            "Waiting jobs",
            &[
                "POS", "JOB", "TENANT", "QUEUE", "GPUS", "PRIO", "WAITING", "REASON",
            ],
            &queue_rows(&v),
            &format!(
                "{} waiting · {} GPUs · oldest {}",
                text(&v, "waiting"),
                text(&v, "gpusWaiting"),
                fmt_secs(num(&v, "oldestWaitSeconds"))
            ),
        ),
        "waits" => show(
            "Queue wait times",
            &["TENANT", "STARTED", "P50", "P90", "MAX"],
            &stats_rows(&v),
            &format!(
                "{} jobs started · p50 {} · p90 {}",
                text(&v["overall"], "started"),
                fmt_secs(num(&v["overall"], "p50Seconds")),
                fmt_secs(num(&v["overall"], "p90Seconds"))
            ),
        ),
        "failures" => show(
            "Failure causes",
            &["CAUSE", "JOBS", "SHARE", "GPUS", "RETRY"],
            &failure_rows(&v),
            &format!(
                "{} failed jobs, {} of them transient",
                text(&v, "total"),
                text(&v, "transient")
            ),
        ),
        "rightsizing" => show(
            "GPU right-sizing",
            &["JOB", "TENANT", "GPUS", "UTIL", "ADVICE", "SAVED GPU-H"],
            &rightsizing_rows(&v),
            &format!(
                "{} of {} jobs with telemetry look oversized · {} GPU-hours could be saved",
                text(&v, "oversized"),
                text(&v, "jobsWithTelemetry"),
                text(&v, "savedGpuHours")
            ),
        ),
        _ => show(
            "Quota use",
            &["TENANT", "QUOTA", "IN USE", "WAITING", "UTIL", "STATE"],
            &quota_rows(&v),
            &format!(
                "{} GPUs in use of {} quota · {} tenant(s) blocked",
                text(&v["totals"], "gpusInUse"),
                text(&v["totals"], "quotaGpus"),
                text(&v["totals"], "blockedTenants")
            ),
        ),
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn cells(rows: Vec<Vec<Cell2>>) -> Vec<Vec<String>> {
        rows.into_iter()
            .map(|r| r.into_iter().map(|c| c.0).collect())
            .collect()
    }

    #[test]
    fn query_values_are_encoded() {
        assert_eq!(enc("gpu-b_1.x"), "gpu-b_1.x");
        assert_eq!(enc("a b&c"), "a%20b%26c");
    }

    #[test]
    fn durations() {
        assert_eq!(fmt_secs(None), "-");
        assert_eq!(fmt_secs(Some(45.0)), "45s");
        assert_eq!(fmt_secs(Some(600.0)), "10m");
        assert_eq!(fmt_secs(Some(12_000.0)), "3h20m");
    }

    #[test]
    fn queue_and_stats_rows() {
        let q = json!({"items": [{"position": 0, "name": "a", "tenant": "t", "queue": "default", "gpus": 2,
            "priority": 5, "waitingSeconds": 300, "reason": "QuotaExhausted"}]});
        assert_eq!(
            cells(queue_rows(&q))[0],
            vec!["0", "a", "t", "default", "2", "5", "5m", "QuotaExhausted"]
        );
        let s = json!({"byTenant": [{"tenant": "t", "started": 3, "p50Seconds": 60, "p90Seconds": 600, "maxSeconds": null}]});
        assert_eq!(cells(stats_rows(&s))[0], vec!["t", "3", "60s", "10m", "-"]);
    }

    #[test]
    fn failure_rows_mark_transient() {
        let f = json!({"categories": [
            {"category": "oom", "jobs": 2, "share": 0.5, "gpus": 4, "transient": false},
            {"category": "preempted", "jobs": 2, "share": 0.5, "gpus": 2, "transient": true}]});
        let rows = failure_rows(&f);
        assert_eq!(
            cells(rows.clone())[0],
            vec!["oom", "2", "50.0%", "4", "will repeat"]
        );
        assert_eq!(rows[1][4].1, Some(Marker::Warn));
    }

    #[test]
    fn rightsizing_and_quota_rows() {
        let r = json!({"items": [
            {"name": "small", "tenant": "a", "gpus": 1, "utilizationPct": 15.0, "savedGpuHours": 7.5,
             "recommendation": {"gpus": 1, "gpuShare": 0.25}},
            {"name": "big", "tenant": "a", "gpus": 8, "utilizationPct": 25.0, "savedGpuHours": 40.0,
             "recommendation": {"gpus": 3, "gpuShare": null}}]});
        let rows = cells(rightsizing_rows(&r));
        assert_eq!(
            rows[0],
            vec!["small", "a", "1", "15.0%", "share 0.25", "7.5"]
        );
        assert_eq!(rows[1][4], "3 GPUs");
        let q = json!({"items": [
            {"tenant": "a", "quotaGpus": 8, "gpusInUse": 8, "gpusWaiting": 1, "utilizationPct": 100.0, "blocked": true},
            {"tenant": "b", "quotaGpus": null, "gpusInUse": 1, "gpusWaiting": 0, "utilizationPct": null}]});
        let rows = quota_rows(&q);
        assert_eq!(rows[0][5], ("blocked".to_string(), Some(Marker::Error)));
        assert_eq!(cells(rows)[1], vec!["b", "-", "1", "0", "-", "ok"]);
    }

    #[test]
    fn empty_bodies_have_no_rows() {
        let e = json!({});
        assert!(
            queue_rows(&e).is_empty() && stats_rows(&e).is_empty() && failure_rows(&e).is_empty()
        );
        assert!(rightsizing_rows(&e).is_empty() && quota_rows(&e).is_empty());
    }
}
