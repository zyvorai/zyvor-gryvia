// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia reservation list|create|cancel`: GryviaReservation objects (cluster-scoped) through the
//! Kubernetes API. The quota operator taints and labels the reserved nodes; deleting the object
//! (cancel) makes its finalizer remove the taint and labels again. Needs the quota operator running
//! with the reservation controller (see docs/gpuaas-completion.md).

use anyhow::{anyhow, bail, Result};
use chrono::{DateTime, Duration, SecondsFormat, Utc};
use dialoguer::Confirm;
use kube::api::{DeleteParams, ListParams, PostParams};
use serde_json::{json, Value};

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

/// Parse `90m`, `4h`, `2d` or `1h30m` into a duration.
pub fn parse_duration(s: &str) -> Result<Duration> {
    let s = s.trim();
    if s.is_empty() {
        bail!("empty duration");
    }
    let mut total = Duration::zero();
    let mut num = String::new();
    for c in s.chars() {
        if c.is_ascii_digit() {
            num.push(c);
            continue;
        }
        let n: i64 = num
            .parse()
            .map_err(|_| anyhow!("invalid duration '{s}': expected e.g. 4h, 90m, 2d, 1h30m"))?;
        num.clear();
        total += match c {
            'm' => Duration::minutes(n),
            'h' => Duration::hours(n),
            'd' => Duration::days(n),
            _ => bail!("invalid duration '{s}': unit '{c}' (use m, h or d)"),
        };
    }
    if !num.is_empty() || total <= Duration::zero() {
        bail!("invalid duration '{s}': expected e.g. 4h, 90m, 2d, 1h30m");
    }
    Ok(total)
}

/// Work out (start, end) from the flags. `--end` and `--duration` are exclusive; one is required.
/// Without `--start` the reservation is immediate (start = None) and a duration counts from `now`.
pub fn resolve_window(
    now: DateTime<Utc>,
    start: Option<&str>,
    end: Option<&str>,
    duration: Option<&str>,
) -> Result<(Option<DateTime<Utc>>, DateTime<Utc>)> {
    let parse = |label: &str, v: &str| {
        DateTime::parse_from_rfc3339(v)
            .map(|t| t.with_timezone(&Utc))
            .map_err(|_| {
                anyhow!("invalid --{label} '{v}': use RFC 3339, e.g. 2026-10-01T09:00:00Z")
            })
    };
    let start = start.map(|v| parse("start", v)).transpose()?;
    let end = match (end, duration) {
        (Some(_), Some(_)) => bail!("use either --end or --duration, not both"),
        (Some(e), None) => parse("end", e)?,
        (None, Some(d)) => start.unwrap_or(now) + parse_duration(d)?,
        (None, None) => bail!("give --end or --duration"),
    };
    if end <= now {
        bail!("the reservation end must be in the future");
    }
    if let Some(s) = start {
        if s >= end {
            bail!("--start must be before the end");
        }
    }
    Ok((start, end))
}

/// The GryviaReservation object `reservation create` submits.
#[allow(clippy::too_many_arguments)]
pub fn build_reservation(
    name: &str,
    owner_type: &str,
    owner: &str,
    gpu_type: &str,
    gpus: u32,
    start: Option<DateTime<Utc>>,
    end: DateTime<Utc>,
    exclusive: bool,
) -> Value {
    let ts = |t: DateTime<Utc>| t.to_rfc3339_opts(SecondsFormat::Secs, true);
    let mut schedule = json!({ "type": if start.is_some() { "scheduled" } else { "immediate" }, "endTime": ts(end) });
    if let Some(s) = start {
        schedule["startTime"] = json!(ts(s));
    }
    json!({
        "apiVersion": "gryvia.io/v1alpha1",
        "kind": "GryviaReservation",
        "metadata": { "name": name },
        "spec": {
            "owner": { "type": owner_type, "name": owner },
            "resources": { "gpuType": gpu_type, "gpuCount": gpus },
            "schedule": schedule,
            "guarantees": { "exclusive": exclusive },
        },
    })
}

fn str_at<'a>(v: &'a Value, ptr: &str) -> &'a str {
    v.pointer(ptr).and_then(Value::as_str).unwrap_or("")
}

fn state_marker(state: &str) -> Marker {
    match state {
        "active" => Marker::Ok,
        "pending" => Marker::Warn,
        "cancelled" => Marker::Error,
        "expired" => Marker::Disabled,
        _ => Marker::Unknown,
    }
}

/// The reservation table with a total line.
pub fn reservation_list_lines(items: &[Value], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("Reservations", color), String::new()];
    if items.is_empty() {
        lines.push("No reservations found".to_string());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = items
        .iter()
        .map(|r| {
            let state = match str_at(r, "/status/state") {
                "" => "-",
                s => s,
            };
            let nodes: Vec<&str> = r
                .pointer("/status/allocatedNodes")
                .and_then(Value::as_array)
                .map(|a| a.iter().filter_map(Value::as_str).collect())
                .unwrap_or_default();
            let want = r
                .pointer("/spec/resources/gpuCount")
                .and_then(Value::as_u64)
                .unwrap_or(0);
            let got = r
                .pointer("/status/allocatedGPUs")
                .and_then(Value::as_u64)
                .unwrap_or(0);
            let end = match str_at(r, "/spec/schedule/endTime") {
                "" => "-",
                e => e,
            };
            vec![
                (str_at(r, "/metadata/name").to_string(), None),
                (
                    format!(
                        "{}/{}",
                        str_at(r, "/spec/owner/type"),
                        str_at(r, "/spec/owner/name")
                    ),
                    None,
                ),
                (
                    format!("{got}/{want} {}", str_at(r, "/spec/resources/gpuType")),
                    None,
                ),
                (state.to_string(), Some(state_marker(state))),
                (
                    if nodes.is_empty() {
                        "-".to_string()
                    } else {
                        nodes.join(",")
                    },
                    None,
                ),
                (end.to_string(), None),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &["NAME", "OWNER", "GPUs", "STATE", "NODES", "ENDS"],
        &rows,
        color,
    ));
    lines.push(String::new());
    lines.push(format!("Total: {} reservations", items.len()));
    lines
}

fn api(client: &GryviaClient) -> kube::Api<kube::core::DynamicObject> {
    super::capacity::gvk_api(client, "GryviaReservation", "gryviareservations")
}

fn api_error(what: &str, e: kube::Error) -> anyhow::Error {
    anyhow!("{what}: {}", display::cluster_error_text(&e.to_string()))
}

pub async fn list(client: &GryviaClient, output: &str) -> Result<()> {
    let items = api(client)
        .list(&ListParams::default())
        .await
        .map_err(|e| api_error("Failed to list reservations", e))?
        .items;
    if output != "table" {
        return crate::output::print_serialized(output, &items);
    }
    let values: Vec<Value> = items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .collect();
    for line in reservation_list_lines(&values, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
pub async fn create(
    client: &GryviaClient,
    name: Option<&str>,
    owner_type: &str,
    owner: &str,
    gpu_type: &str,
    gpus: u32,
    start: Option<&str>,
    end: Option<&str>,
    duration: Option<&str>,
    exclusive: bool,
) -> Result<()> {
    if gpus == 0 {
        bail!("--gpus must be at least 1");
    }
    let now = Utc::now();
    let (start, end) = resolve_window(now, start, end, duration)?;
    let generated = format!("resv-{}", now.timestamp());
    let name = name.unwrap_or(&generated);
    crate::commands::tenant::validate_name(name)?;
    let body = build_reservation(
        name, owner_type, owner, gpu_type, gpus, start, end, exclusive,
    );
    let obj: kube::core::DynamicObject = serde_json::from_value(body)?;
    api(client)
        .create(&PostParams::default(), &obj)
        .await
        .map_err(|e| api_error(&format!("Failed to create reservation '{name}'"), e))?;
    display::print_success(&format!(
        "Reservation {name} created; jobs opt in with the annotation gryvia.io/reservation={name}"
    ));
    Ok(())
}

pub async fn cancel(client: &GryviaClient, name: &str, yes: bool) -> Result<()> {
    if !yes {
        let confirm = Confirm::new()
            .with_prompt(format!(
                "Cancel reservation '{name}'? The nodes are released; running jobs are not stopped"
            ))
            .interact()?;
        if !confirm {
            display::print_info("Cancelled");
            return Ok(());
        }
    }
    api(client)
        .delete(name, &DeleteParams::default())
        .await
        .map_err(|e| api_error(&format!("Failed to cancel reservation '{name}'"), e))?;
    display::print_success(&format!("Reservation {name} cancelled"));
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn now() -> DateTime<Utc> {
        DateTime::parse_from_rfc3339("2030-01-01T00:00:00Z")
            .unwrap()
            .with_timezone(&Utc)
    }

    #[test]
    fn durations_parse() {
        assert_eq!(parse_duration("90m").unwrap(), Duration::minutes(90));
        assert_eq!(parse_duration("1h30m").unwrap(), Duration::minutes(90));
        assert_eq!(parse_duration("2d").unwrap(), Duration::days(2));
        for bad in ["", "h", "10", "5x", "0h", "1.5h"] {
            assert!(parse_duration(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn window_rules() {
        let (s, e) = resolve_window(now(), None, None, Some("4h")).unwrap();
        assert!(s.is_none());
        assert_eq!(e, now() + Duration::hours(4));
        let (s, e) = resolve_window(now(), Some("2030-01-02T00:00:00Z"), None, Some("1h")).unwrap();
        assert_eq!(e, s.unwrap() + Duration::hours(1));
        assert!(resolve_window(now(), None, None, None).is_err());
        assert!(resolve_window(now(), None, Some("2030-02-01T00:00:00Z"), Some("1h")).is_err());
        assert!(resolve_window(now(), None, Some("2029-01-01T00:00:00Z"), None).is_err());
        assert!(resolve_window(
            now(),
            Some("2030-03-01T00:00:00Z"),
            Some("2030-02-01T00:00:00Z"),
            None
        )
        .is_err());
        assert!(resolve_window(now(), Some("tomorrow"), None, Some("1h")).is_err());
    }

    #[test]
    fn build_immediate_and_scheduled() {
        let end = now() + Duration::hours(4);
        let r = build_reservation("r1", "team", "ml", "H100", 8, None, end, true);
        assert_eq!(r["kind"], "GryviaReservation");
        assert_eq!(r["spec"]["owner"]["name"], "ml");
        assert_eq!(r["spec"]["resources"]["gpuCount"], 8);
        assert_eq!(r["spec"]["schedule"]["type"], "immediate");
        assert_eq!(r["spec"]["schedule"]["endTime"], "2030-01-01T04:00:00Z");
        assert!(r["spec"]["schedule"].get("startTime").is_none());
        assert_eq!(r["spec"]["guarantees"]["exclusive"], true);
        let r = build_reservation("r1", "team", "ml", "H100", 8, Some(now()), end, false);
        assert_eq!(r["spec"]["schedule"]["type"], "scheduled");
        assert_eq!(r["spec"]["schedule"]["startTime"], "2030-01-01T00:00:00Z");
    }

    #[test]
    fn list_report_aligned() {
        let items = vec![
            json!({"metadata": {"name": "r1"}, "spec": {"owner": {"type": "team", "name": "ml"},
                "resources": {"gpuType": "H100", "gpuCount": 16}, "schedule": {"endTime": "2030-01-01T04:00:00Z"}},
                "status": {"state": "active", "allocatedNodes": ["n1", "n2"], "allocatedGPUs": 16}}),
            json!({"metadata": {"name": "r2"}, "spec": {"owner": {"type": "user", "name": "bob"},
                "resources": {"gpuType": "A100", "gpuCount": 8}, "schedule": {}}}),
        ];
        let text = reservation_list_lines(&items, false).join("\n");
        let expected = "\
━━━ Reservations ━━━

NAME  OWNER     GPUs        STATE   NODES  ENDS
r1    team/ml   16/16 H100  active  n1,n2  2030-01-01T04:00:00Z
r2    user/bob  0/8 A100    -       -      -

Total: 2 reservations";
        assert_eq!(text, expected);
        assert!(reservation_list_lines(&[], false)
            .join("\n")
            .contains("No reservations found"));
    }
}
