// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia list experiments` / `gryvia get experiment <name>`: GryviaLiveExperiment leaderboards.
//!
//! Read-only. The ai-operator owns the status; this only shows it, best run first, with the gap to the best.

use anyhow::{Context, Result};
use kube::api::{Api, ApiResource, GroupVersionKind, ListParams};
use kube::core::DynamicObject;

use crate::client::GryviaClient;
use crate::ui::{self, Cell2, Marker};

fn api(client: &GryviaClient, all_namespaces: bool) -> Api<DynamicObject> {
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaLiveExperiment",
    ));
    if all_namespaces {
        Api::all_with(client.kube_client.clone(), &ar)
    } else {
        Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar)
    }
}

pub async fn list(client: &GryviaClient, all_namespaces: bool, output: &str) -> Result<()> {
    let items = api(client, all_namespaces)
        .list(&ListParams::default())
        .await
        .context("Failed to list experiments")?;
    match output {
        "json" => println!("{}", serde_json::to_string_pretty(&items)?),
        "yaml" => println!("{}", serde_yaml::to_string(&items)?),
        _ => {
            for line in experiments_lines(&items.items, ui::color_enabled()) {
                println!("{line}");
            }
        }
    }
    Ok(())
}

pub async fn get(client: &GryviaClient, name: &str, output: &str) -> Result<()> {
    let exp = api(client, false)
        .get(name)
        .await
        .context("Failed to get experiment")?;
    match output {
        "json" => println!("{}", serde_json::to_string_pretty(&exp)?),
        _ => println!("{}", serde_yaml::to_string(&exp)?),
    }
    Ok(())
}

fn text<'a>(obj: &'a DynamicObject, path: &[&str]) -> Option<&'a str> {
    let mut v = Some(&obj.data);
    for key in path {
        v = v.and_then(|x| x.get(*key));
    }
    v.and_then(|x| x.as_str()).filter(|s| !s.is_empty())
}

/// The best leaderboard entry as (job, value), judged by `spec.comparison.direction`
/// (`minimize` or `maximize`). None when there is no numeric entry or the direction is unknown.
pub fn leader(obj: &DynamicObject) -> Option<(String, f64)> {
    let dir = text(obj, &["spec", "comparison", "direction"])?;
    let board = obj.data.get("status")?.get("leaderboard")?.as_array()?;
    let mut best: Option<(String, f64)> = None;
    for e in board {
        let (Some(job), Some(v)) = (
            e.get("job").and_then(|j| j.as_str()),
            e.get("primaryMetricValue").and_then(|v| v.as_f64()),
        ) else {
            continue;
        };
        let better = match (&best, dir) {
            (None, "minimize" | "maximize") => true,
            (Some((_, b)), "minimize") => v < *b,
            (Some((_, b)), "maximize") => v > *b,
            _ => false,
        };
        if better {
            best = Some((job.to_string(), v));
        }
    }
    best
}

fn phase_marker(phase: &str) -> Marker {
    match phase {
        "Completed" | "Succeeded" => Marker::Ok,
        "Running" | "Pending" => Marker::Warn,
        "Failed" => Marker::Error,
        _ => Marker::Unknown,
    }
}

/// Lines of the experiments table.
pub fn experiments_lines(items: &[DynamicObject], color: bool) -> Vec<String> {
    if items.is_empty() {
        return vec![Marker::Disabled.paint_with("No experiments found.", color)];
    }
    let rows: Vec<Vec<Cell2>> = items
        .iter()
        .map(|o| {
            let phase = o
                .data
                .get("status")
                .and_then(|s| s.get("phase"))
                .and_then(|p| p.as_str())
                .unwrap_or("Pending");
            let (lead, value) = match leader(o) {
                Some((j, v)) => (j, format!("{v}")),
                None => ("-".to_string(), "-".to_string()),
            };
            let jobs = o
                .data
                .get("spec")
                .and_then(|s| s.get("jobs"))
                .and_then(|j| j.as_array())
                .map(|a| a.len())
                .unwrap_or(0);
            vec![
                (
                    o.metadata
                        .name
                        .as_deref()
                        .unwrap_or("<unknown>")
                        .to_string(),
                    None,
                ),
                (
                    text(o, &["spec", "comparison", "primaryMetric"])
                        .unwrap_or("-")
                        .to_string(),
                    None,
                ),
                (
                    text(o, &["spec", "comparison", "direction"])
                        .unwrap_or("-")
                        .to_string(),
                    None,
                ),
                (jobs.to_string(), None),
                (lead, None),
                (value, None),
                (phase.to_string(), Some(phase_marker(phase))),
            ]
        })
        .collect();
    ui::grid(
        &[
            "NAME",
            "METRIC",
            "DIRECTION",
            "JOBS",
            "LEADER",
            "BEST",
            "STATUS",
        ],
        &rows,
        color,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::commands::network::strip_ansi;
    use serde_json::json;

    fn exp(name: &str, dir: &str, board: serde_json::Value, phase: &str) -> DynamicObject {
        serde_json::from_value(json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaLiveExperiment",
            "metadata": {"name": name},
            "spec": {"comparison": {"primaryMetric": "loss", "direction": dir}, "jobs": [{"name": "a", "jobRef": "ja"}, {"name": "b", "jobRef": "jb"}]},
            "status": {"phase": phase, "leaderboard": board},
        }))
        .unwrap()
    }

    #[test]
    fn leader_follows_direction_not_operator_rank() {
        let board = json!([{"rank": 1, "job": "a", "primaryMetricValue": 0.5}, {"rank": 2, "job": "b", "primaryMetricValue": 0.4}]);
        assert_eq!(
            leader(&exp("e", "minimize", board.clone(), "Running")),
            Some(("b".into(), 0.4))
        );
        assert_eq!(
            leader(&exp("e", "maximize", board.clone(), "Running")),
            Some(("a".into(), 0.5))
        );
        assert_eq!(leader(&exp("e", "sideways", board, "Running")), None);
        assert_eq!(leader(&exp("e", "minimize", json!([]), "Running")), None);
    }

    #[test]
    fn table_shows_leader_and_empty_state() {
        let board = json!([{"rank": 1, "job": "a", "primaryMetricValue": 0.5}, {"rank": 2, "job": "b", "primaryMetricValue": 0.4}]);
        let lines = experiments_lines(&[exp("lr-sweep", "minimize", board, "Running")], false);
        let text = strip_ansi(&lines.join("\n"));
        assert!(
            text.contains("lr-sweep") && text.contains("loss") && text.contains("minimize"),
            "{text}"
        );
        assert!(
            text.contains("b") && text.contains("0.4") && text.contains("Running"),
            "{text}"
        );
        let empty = experiments_lines(&[], false);
        assert!(strip_ansi(&empty.join("\n")).contains("No experiments found"));
    }
}
