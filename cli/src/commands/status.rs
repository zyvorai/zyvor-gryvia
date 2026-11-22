// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use kube::api::Api;
use tokio::time::{sleep, Duration};

use crate::client::GryviaClient;
use crate::types::*;
use crate::ui::{self, Marker};

pub async fn execute(client: &GryviaClient, job: &str, follow: bool) -> Result<()> {
    if follow {
        loop {
            // Clear screen using ANSI escape sequences. This works on POSIX-compliant
            // terminals but may render as garbage on non-ANSI terminals (e.g. Windows cmd.exe
            // without virtual terminal processing enabled).
            print!("\x1B[2J\x1B[1;1H");
            let api: Api<GryviaAIJob> =
                Api::namespaced(client.kube_client.clone(), client.namespace());

            let job_obj = api.get(job).await.context("Failed to get job")?;

            print_job_status(&job_obj);

            let status = job_obj.status.clone().unwrap_or_default();
            match status.phase.as_str() {
                "Completed" | "Succeeded" | "Failed" => {
                    break;
                }
                _ => {}
            }

            sleep(Duration::from_secs(5)).await;
        }
    } else {
        let api: Api<GryviaAIJob> = Api::namespaced(client.kube_client.clone(), client.namespace());

        let job_obj = api.get(job).await.context("Failed to get job")?;

        print_job_status(&job_obj);
    }

    Ok(())
}

fn print_job_status(job: &GryviaAIJob) {
    for line in job_status_lines(job, chrono::Utc::now(), ui::color_enabled()) {
        println!("{line}");
    }
}

fn hms(d: chrono::Duration) -> String {
    format!(
        "{}h {}m {}s",
        d.num_hours(),
        d.num_minutes() % 60,
        d.num_seconds() % 60
    )
}

/// One job's status report. `now` is only used to show how long a running job has been going.
pub fn job_status_lines(
    job: &GryviaAIJob,
    now: chrono::DateTime<chrono::Utc>,
    color: bool,
) -> Vec<String> {
    let unknown = "<unknown>".to_string();
    let name = job.metadata.name.as_ref().unwrap_or(&unknown);
    let status = job.status.clone().unwrap_or_default();
    let phase = &status.phase;
    let kv = |key: &str, value: &str| format!("  {}", ui::kv(key, value, 12, color));

    let mut lines = vec![
        ui::header(&format!("Job: {name}"), color),
        String::new(),
        ui::kv("Type", &job.spec.job_type, 7, color),
        ui::kv("Image", &job.spec.image, 7, color),
        String::new(),
        ui::section("Resources", color),
        kv("GPU Type", &job.spec.gpu_type),
        kv("GPU Count", &job.spec.gpus.to_string()),
        kv("Memory", &job.spec.resources.request("memory")),
        kv("CPU", &job.spec.resources.request("cpu")),
        String::new(),
    ];

    if job.spec.distributed.enabled {
        lines.push(ui::section("Distributed Training", color));
        lines.push(kv("Framework", &job.spec.distributed.framework));
        lines.push(kv("Nodes", &job.spec.distributed.nodes.to_string()));
        lines.push(kv(
            "GPUs/Node",
            &job.spec.distributed.gpus_per_node.to_string(),
        ));
        lines.push(String::new());
    }

    lines.push(ui::section("Status", color));
    lines.push(kv(
        "Phase",
        &Marker::from_phase(phase).paint_with(phase, color),
    ));
    if !status.message.is_empty() {
        lines.push(kv("Message", &status.message));
    }
    if let Some(start) = status.start_time {
        lines.push(kv("Started", &start.to_string()));
        if let Some(completion) = status.completion_time {
            lines.push(kv("Completed", &completion.to_string()));
            lines.push(kv(
                "Duration",
                &hms(completion.signed_duration_since(start)),
            ));
        } else if phase == "Running" {
            lines.push(kv("Running for", &hms(now.signed_duration_since(start))));
        }
    }
    lines.push(String::new());
    lines
}

#[cfg(test)]
mod tests {
    use super::*;

    fn job(status: serde_json::Value) -> GryviaAIJob {
        serde_json::from_value(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaAIJob",
            "metadata": {"name": "llama-ft"},
            "spec": {
                "type": "pytorch", "gpus": 16, "gpuType": "H100", "image": "reg/train:1",
                "resources": {"requests": {"memory": "64Gi", "cpu": 8}},
                "distributed": {"enabled": true, "framework": "torchrun", "backend": "nccl", "nodes": 2, "gpusPerNode": 8}
            },
            "status": status
        }))
        .unwrap()
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

    fn now() -> chrono::DateTime<chrono::Utc> {
        chrono::DateTime::parse_from_rfc3339("2026-09-29T12:30:05Z")
            .unwrap()
            .with_timezone(&chrono::Utc)
    }

    #[test]
    fn running_job_aligned() {
        let j = job(serde_json::json!({"phase": "Running", "startTime": "2026-09-29T10:00:00Z"}));
        let text = job_status_lines(&j, now(), false).join("\n");
        let expected = "\
━━━ Job: llama-ft ━━━

Type   pytorch
Image  reg/train:1

Resources
  GPU Type    H100
  GPU Count   16
  Memory      64Gi
  CPU         8

Distributed Training
  Framework   torchrun
  Nodes       2
  GPUs/Node   8

Status
  Phase       Running
  Started     2026-09-29 10:00:00 UTC
  Running for 2h 30m 5s
";
        assert_eq!(text, expected);
    }

    #[test]
    fn finished_job_shows_duration_and_message() {
        let j = job(serde_json::json!({
            "phase": "Failed", "message": "OOMKilled",
            "startTime": "2026-09-29T10:00:00Z", "completionTime": "2026-09-29T11:01:02Z"
        }));
        let text = job_status_lines(&j, now(), false).join("\n");
        assert!(text.contains("  Phase       Failed\n  Message     OOMKilled\n"));
        assert!(text.contains("  Duration    1h 1m 2s"));
        assert!(!text.contains("Running for"));
    }

    #[test]
    fn color_output_strips_to_plain() {
        let j = job(serde_json::json!({"phase": "Pending"}));
        let colored = job_status_lines(&j, now(), true).join("\n");
        assert!(colored.contains("\x1b["));
        assert_eq!(
            strip(&colored),
            job_status_lines(&j, now(), false).join("\n")
        );
    }
}
