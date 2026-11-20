// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

use anyhow::{Context, Result};
use kube::api::{Api, ListParams};

use crate::client::GryviaClient;
use crate::display;
use crate::types::*;
use crate::ui::{self, Marker};

pub async fn execute(
    client: &GryviaClient,
    team: Option<String>,
    budget: bool,
    output: &str,
) -> Result<()> {
    let api: Api<GryviaQuota> = Api::all(client.kube_client.clone());

    let quotas = api
        .list(&ListParams::default())
        .await
        .context("Failed to list quotas")?;

    let filtered: Vec<_> = if let Some(team_name) = team {
        quotas
            .items
            .iter()
            .filter(|q| q.spec.team == team_name)
            .collect()
    } else {
        quotas.items.iter().collect()
    };

    if output != "table" {
        return crate::output::print_serialized(output, &filtered);
    }

    if filtered.is_empty() {
        display::print_warning("No quotas found");
        return Ok(());
    }

    for quota in filtered {
        for line in quota_lines(quota, budget, ui::color_enabled()) {
            println!("{line}");
        }
        println!();
    }

    Ok(())
}

/// Marker for a percentage of a limit used: 100% and above is an error, 80% and above a warning.
fn usage_marker(percent: f64) -> Marker {
    if percent >= 100.0 {
        Marker::Error
    } else if percent >= 80.0 {
        Marker::Warn
    } else {
        Marker::Ok
    }
}

/// One team's quota report.
pub fn quota_lines(quota: &GryviaQuota, show_budget: bool, color: bool) -> Vec<String> {
    const W: usize = 24;
    let unknown = "<unknown>".to_string();
    let quota_name = quota.metadata.name.as_ref().unwrap_or(&unknown);
    let quota_status = quota.status.clone().unwrap_or_default();
    let usage = &quota_status.current_usage;
    let spec = &quota.spec.gpu_quota;
    let row = |key: &str, value: String| format!("  {}", ui::kv(key, &value, W, color));

    let mut lines = vec![
        ui::header(&format!("Quota: {}", quota.spec.team), color),
        String::new(),
        ui::kv("Quota", quota_name, 8, color),
        ui::kv("Team", &quota.spec.team, 8, color),
        String::new(),
        ui::section("GPU Quota", color),
    ];

    let gpu_percent = if spec.max_gpus > 0 {
        usage.allocated_gpus as f64 / spec.max_gpus as f64 * 100.0
    } else {
        0.0
    };
    lines.push(row(
        "Allocated GPUs",
        format!(
            "{}/{} ({}%)",
            usage_marker(gpu_percent).paint_with(&usage.allocated_gpus.to_string(), color),
            spec.max_gpus,
            gpu_percent.round() as i32
        ),
    ));
    lines.push(row("Max GPUs per Job", spec.max_gpus_per_job.to_string()));
    lines.push(row(
        "Running Jobs",
        format!("{}/{}", usage.running_jobs, spec.max_running_jobs),
    ));
    lines.push(row("Queued Jobs", usage.queued_jobs.to_string()));
    if !spec.allowed_gpu_types.is_empty() {
        lines.push(row("Allowed GPU Types", spec.allowed_gpu_types.join(", ")));
    }
    lines.push(row(
        "GPU Hours (this month)",
        format!("{:.2}", usage.gpu_hours),
    ));

    if show_budget {
        if let Some(ref bs) = quota_status.budget_status {
            lines.push(String::new());
            lines.push(ui::section("Budget Status", color));
            let percent = bs.percent_used;
            let marker = usage_marker(percent);
            lines.push(row("Spent", format!("${:.2}", bs.spent_this_month)));
            lines.push(row(
                "Budget",
                format!(
                    "${:.2}",
                    quota
                        .spec
                        .budget
                        .as_ref()
                        .map(|b| b.monthly_budget)
                        .unwrap_or(0.0)
                ),
            ));
            lines.push(row("Remaining", format!("${:.2}", bs.remaining_budget)));
            lines.push(row(
                "Used",
                marker.paint_with(&format!("{:.1}%", percent), color),
            ));
            lines.push(row("Projected", format!("${:.2}", bs.projected_spend)));

            if let Some(ref budget) = quota.spec.budget {
                if budget.hard_limit && percent >= 100.0 {
                    lines.push(format!(
                        "  {} {}",
                        Marker::Error.paint_with(Marker::Error.glyph(), color),
                        Marker::Error
                            .paint_with("HARD LIMIT REACHED - New jobs will be blocked", color)
                    ));
                } else if percent >= budget.alert_threshold * 100.0 {
                    lines.push(format!(
                        "  {} {}",
                        Marker::Warn.paint_with(Marker::Warn.glyph(), color),
                        Marker::Warn.paint_with(
                            &format!(
                                "Alert threshold ({:.0}%) reached",
                                budget.alert_threshold * 100.0
                            ),
                            color
                        )
                    ));
                }
            }
        }
    }

    lines.push(String::new());
    let phase = &quota_status.phase;
    lines.push(ui::kv(
        "Status",
        &Marker::from_phase(phase).paint_with(phase, color),
        8,
        color,
    ));
    lines
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> GryviaQuota {
        serde_json::from_value(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaQuota",
            "metadata": {"name": "ml-quota"},
            "spec": {
                "team": "ml", "namespaces": ["ml"],
                "gpuQuota": {"maxGPUs": 16, "maxGPUsPerJob": 8, "allowedGPUTypes": ["H100", "A100"], "maxRunningJobs": 4},
                "budget": {"monthlyBudget": 1000.0, "alertThreshold": 0.8, "hardLimit": true}
            },
            "status": {
                "phase": "Active",
                "currentUsage": {"allocatedGPUs": 12, "runningJobs": 2, "queuedJobs": 1, "gpuHours": 345.678},
                "budgetStatus": {"spentThisMonth": 850.5, "remainingBudget": 149.5, "percentUsed": 85.05, "projectedSpend": 1100.0}
            }
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

    #[test]
    fn quota_report_aligned() {
        let text = quota_lines(&fixture(), true, false).join("\n");
        let expected = "\
━━━ Quota: ml ━━━

Quota   ml-quota
Team    ml

GPU Quota
  Allocated GPUs          12/16 (75%)
  Max GPUs per Job        8
  Running Jobs            2/4
  Queued Jobs             1
  Allowed GPU Types       H100, A100
  GPU Hours (this month)  345.68

Budget Status
  Spent                   $850.50
  Budget                  $1000.00
  Remaining               $149.50
  Used                    85.0%
  Projected               $1100.00
  ! Alert threshold (80%) reached

Status  Active";
        assert_eq!(text, expected);
    }

    #[test]
    fn budget_section_is_opt_in() {
        let text = quota_lines(&fixture(), false, false).join("\n");
        assert!(!text.contains("Budget Status"));
    }

    #[test]
    fn color_output_strips_to_plain() {
        let colored = quota_lines(&fixture(), true, true).join("\n");
        assert!(colored.contains("\x1b["));
        assert_eq!(
            strip(&colored),
            quota_lines(&fixture(), true, false).join("\n")
        );
    }
}
