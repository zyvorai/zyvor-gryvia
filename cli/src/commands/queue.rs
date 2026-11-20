// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

use anyhow::{Context, Result};
use kube::api::{Api, ListParams};
use tokio::time::{sleep, Duration};

use crate::client::GryviaClient;
use crate::display;
use crate::types::*;
use crate::ui::{self, Cell2, Marker};

pub async fn execute(
    client: &GryviaClient,
    name: Option<String>,
    watch: Option<u64>,
    output: &str,
) -> Result<()> {
    if let Some(interval) = watch {
        if interval == 0 {
            anyhow::bail!("Watch interval must be greater than 0");
        }
        if output != "table" {
            anyhow::bail!("--watch needs the table output format");
        }
        loop {
            print!("\x1B[2J\x1B[1;1H"); // Clear screen
            if let Err(e) = show_queue(client, &name, output).await {
                eprintln!("Error refreshing queue: {}", e);
            }
            sleep(Duration::from_secs(interval)).await;
        }
    } else {
        show_queue(client, &name, output).await?;
    }

    Ok(())
}

/// Pending/queued/scheduling jobs, optionally only those whose name contains `filter`.
fn queued_jobs<'a>(jobs: &'a [GryviaAIJob], filter: Option<&str>) -> Vec<&'a GryviaAIJob> {
    jobs.iter()
        .filter(|j| {
            let phase = j.status.as_ref().map(|s| s.phase.as_str()).unwrap_or("");
            matches!(phase, "Pending" | "Queued" | "Scheduling")
        })
        .filter(|j| match filter {
            Some(f) => j.metadata.name.as_deref().unwrap_or("").contains(f),
            None => true,
        })
        .collect()
}

fn running_jobs(jobs: &[GryviaAIJob]) -> Vec<&GryviaAIJob> {
    jobs.iter()
        .filter(|j| j.status.as_ref().map(|s| s.phase.as_str()) == Some("Running"))
        .collect()
}

fn duration_short(d: chrono::Duration, with_seconds: bool) -> String {
    if d.num_hours() > 0 {
        format!("{}h{}m", d.num_hours(), d.num_minutes() % 60)
    } else if d.num_minutes() > 0 || !with_seconds {
        format!("{}m", d.num_minutes())
    } else {
        format!("{}s", d.num_seconds())
    }
}

/// The queue report. Ends after the summary when nothing is queued.
pub fn queue_lines(
    jobs: &[GryviaAIJob],
    filter: Option<&str>,
    now: chrono::DateTime<chrono::Utc>,
    color: bool,
) -> Vec<String> {
    let queued = queued_jobs(jobs, filter);
    let running = running_jobs(jobs);
    let unknown = "<unknown>".to_string();

    let mut lines = vec![ui::header("Job Queue", color), String::new()];
    lines.push(ui::section("Queue Summary", color));
    lines.push(format!(
        "  {}",
        ui::kv(
            "Queued/Pending",
            &Marker::Warn.paint_with(&queued.len().to_string(), color),
            16,
            color
        )
    ));
    lines.push(format!(
        "  {}",
        ui::kv(
            "Running",
            &Marker::Ok.paint_with(&running.len().to_string(), color),
            16,
            color
        )
    ));
    lines.push(String::new());
    if queued.is_empty() {
        return lines;
    }

    lines.push(ui::section("Queued Jobs", color));
    let rows: Vec<Vec<Cell2>> = queued
        .iter()
        .enumerate()
        .map(|(i, job)| {
            let phase = job.status.clone().unwrap_or_default().phase;
            let waiting = match job.metadata.creation_timestamp {
                Some(ref ts) => {
                    let created =
                        chrono::DateTime::from_timestamp(ts.0.as_second(), 0).unwrap_or_default();
                    duration_short(now.signed_duration_since(created), true)
                }
                None => "-".to_string(),
            };
            vec![
                (format!("#{}", i + 1), None),
                (job.metadata.name.as_ref().unwrap_or(&unknown).clone(), None),
                (job.spec.job_type.clone(), None),
                (job.spec.gpus.to_string(), None),
                (job.spec.gpu_type.clone(), None),
                (phase.clone(), Some(Marker::from_phase(&phase))),
                (waiting, None),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &[
            "POSITION",
            "NAME",
            "FRAMEWORK",
            "GPUs",
            "GPU TYPE",
            "STATUS",
            "WAITING",
        ],
        &rows,
        color,
    ));
    lines.push(String::new());

    if !running.is_empty() {
        lines.push(ui::section("Running Jobs", color));
        let rows: Vec<Vec<Cell2>> = running
            .iter()
            .map(|job| {
                let running_for = match job.status.as_ref().and_then(|s| s.start_time) {
                    Some(start) => duration_short(now.signed_duration_since(start), false),
                    None => "-".to_string(),
                };
                vec![
                    (job.metadata.name.as_ref().unwrap_or(&unknown).clone(), None),
                    (job.spec.gpus.to_string(), None),
                    (job.spec.gpu_type.clone(), None),
                    (running_for, None),
                ]
            })
            .collect();
        lines.extend(ui::grid(
            &["NAME", "GPUs", "GPU TYPE", "RUNNING FOR"],
            &rows,
            color,
        ));
        lines.push(String::new());
    }

    let queued_gpus: u32 = queued.iter().map(|j| j.spec.gpus).sum();
    let running_gpus: u32 = running.iter().map(|j| j.spec.gpus).sum();
    lines.push(ui::section("GPU Demand", color));
    for (key, value) in [
        (
            "Running",
            Marker::Ok.paint_with(&format!("{running_gpus} GPUs"), color),
        ),
        (
            "Queued",
            Marker::Warn.paint_with(&format!("{queued_gpus} GPUs"), color),
        ),
        (
            "Total",
            ui::ansi(&format!("{} GPUs", running_gpus + queued_gpus), "1", color),
        ),
    ] {
        lines.push(format!("  {}", ui::kv(key, &value, 9, color)));
    }
    lines
}

async fn show_queue(client: &GryviaClient, name: &Option<String>, output: &str) -> Result<()> {
    let api: Api<GryviaAIJob> = Api::all(client.kube_client.clone());

    let jobs = api
        .list(&ListParams::default())
        .await
        .context("Failed to list jobs")?;

    if output != "table" {
        let queued = queued_jobs(&jobs.items, name.as_deref());
        return crate::output::print_serialized(output, &queued);
    }

    for line in queue_lines(
        &jobs.items,
        name.as_deref(),
        chrono::Utc::now(),
        ui::color_enabled(),
    ) {
        println!("{line}");
    }
    if queued_jobs(&jobs.items, name.as_deref()).is_empty() {
        display::print_info("No jobs in queue");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn job(
        name: &str,
        phase: &str,
        gpus: u32,
        created: &str,
        started: Option<&str>,
    ) -> GryviaAIJob {
        let mut status = serde_json::json!({"phase": phase});
        if let Some(s) = started {
            status["startTime"] = s.into();
        }
        serde_json::from_value(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaAIJob",
            "metadata": {"name": name, "creationTimestamp": created},
            "spec": {"type": "pytorch", "gpus": gpus, "gpuType": "H100"},
            "status": status
        }))
        .unwrap()
    }

    fn now() -> chrono::DateTime<chrono::Utc> {
        chrono::DateTime::parse_from_rfc3339("2026-09-29T12:00:00Z")
            .unwrap()
            .with_timezone(&chrono::Utc)
    }

    fn fixture() -> Vec<GryviaAIJob> {
        vec![
            job("train-a", "Pending", 8, "2026-09-29T11:30:00Z", None),
            job("train-bb", "Queued", 4, "2026-09-29T09:15:00Z", None),
            job(
                "serve-1",
                "Running",
                2,
                "2026-09-29T08:00:00Z",
                Some("2026-09-29T10:00:00Z"),
            ),
        ]
    }

    fn strip(s: &str) -> String {
        let mut out = String::new();
        let mut esc = false;
        for c in s.chars() {
            match (esc, c) {
                (false, '\x1b') => esc = true,
                (true, 'm') => esc = false,
                (false, c) => out.push(c),
                _ => {}
            }
        }
        out
    }

    #[test]
    fn queue_report_aligned() {
        let text = queue_lines(&fixture(), None, now(), false).join("\n");
        let expected = "\
━━━ Job Queue ━━━

Queue Summary
  Queued/Pending  2
  Running         1

Queued Jobs
POSITION  NAME      FRAMEWORK  GPUs  GPU TYPE  STATUS   WAITING
#1        train-a   pytorch    8     H100      Pending  30m
#2        train-bb  pytorch    4     H100      Queued   2h45m

Running Jobs
NAME     GPUs  GPU TYPE  RUNNING FOR
serve-1  2     H100      2h0m

GPU Demand
  Running  2 GPUs
  Queued   12 GPUs
  Total    14 GPUs";
        assert_eq!(text, expected);
    }

    #[test]
    fn empty_queue_stops_after_summary_and_filter_applies() {
        let lines = queue_lines(&fixture(), Some("nomatch"), now(), false);
        assert_eq!(lines.last().map(String::as_str), Some(""));
        assert!(!lines.join("\n").contains("Queued Jobs"));
        assert_eq!(queued_jobs(&fixture(), Some("bb")).len(), 1);
    }

    #[test]
    fn color_output_strips_to_plain() {
        let colored = queue_lines(&fixture(), None, now(), true).join("\n");
        assert!(colored.contains("\x1b["));
        assert_eq!(
            strip(&colored),
            queue_lines(&fixture(), None, now(), false).join("\n")
        );
    }
}
