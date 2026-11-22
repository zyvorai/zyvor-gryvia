// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use kube::api::{Api, ListParams};

use crate::client::GryviaClient;
use crate::types::*;
use crate::ui::{self, Cell2, Marker};

pub async fn execute(
    client: &GryviaClient,
    team: Option<String>,
    period: &str,
    detailed: bool,
) -> Result<()> {
    // Validate period
    match period {
        "day" | "week" | "month" => {}
        _ => anyhow::bail!(
            "Invalid period: '{}'. Valid periods: day, week, month",
            period
        ),
    }
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

    let color = ui::color_enabled();
    for line in cost_lines(&filtered, period, color) {
        println!("{line}");
    }
    if detailed {
        println!();
        for line in breakdown_lines(&filtered, color) {
            println!("{line}");
        }
    }

    Ok(())
}

/// Marker for the share of a budget already spent.
fn percent_marker(percent: f64) -> Marker {
    if percent >= 100.0 {
        Marker::Error
    } else if percent >= 80.0 {
        Marker::Warn
    } else {
        Marker::Ok
    }
}

/// The per-team spend table and the summary under it.
pub fn cost_lines(quotas: &[&GryviaQuota], period: &str, color: bool) -> Vec<String> {
    let mut lines = vec![
        ui::header(&format!("Cost Analysis ({period})"), color),
        String::new(),
    ];

    let mut total_spent = 0.0;
    let mut total_budget = 0.0;
    let mut rows: Vec<Vec<Cell2>> = Vec::new();
    for quota in quotas {
        let quota_status = quota.status.clone().unwrap_or_default();
        if let Some(ref bs) = quota_status.budget_status {
            let budget = quota
                .spec
                .budget
                .as_ref()
                .map(|b| b.monthly_budget)
                .unwrap_or(0.0);
            total_spent += bs.spent_this_month;
            total_budget += budget;
            rows.push(vec![
                (quota.spec.team.clone(), None),
                (format!("${:.2}", bs.spent_this_month), None),
                (format!("${:.2}", budget), None),
                (format!("${:.2}", bs.remaining_budget), None),
                (
                    format!("{:.1}%", bs.percent_used),
                    Some(percent_marker(bs.percent_used)),
                ),
                (format!("${:.2}", bs.projected_spend), None),
            ]);
        }
    }
    lines.extend(ui::grid(
        &[
            "TEAM",
            "SPENT",
            "BUDGET",
            "REMAINING",
            "% USED",
            "PROJECTED",
        ],
        &rows,
        color,
    ));

    lines.push(String::new());
    lines.push(ui::section("Summary", color));
    let overall = if total_budget > 0.0 {
        total_spent / total_budget * 100.0
    } else {
        0.0
    };
    for (key, value) in [
        (
            "Total Spent",
            Marker::Warn.paint_with(&format!("${:.2}", total_spent), color),
        ),
        ("Total Budget", format!("${:.2}", total_budget)),
        (
            "Total Remaining",
            Marker::Ok.paint_with(&format!("${:.2}", total_budget - total_spent), color),
        ),
        ("Overall Usage", format!("{:.1}%", overall)),
    ] {
        lines.push(format!("  {}", ui::kv(key, &value, 17, color)));
    }
    lines
}

/// `--detailed`: GPU pricing and one block per team.
pub fn breakdown_lines(quotas: &[&GryviaQuota], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("Detailed Breakdown", color), String::new()];

    // GPU hourly rates
    let gpu_rates = [
        ("H100", 3.50),
        ("A100-80G", 2.21),
        ("A100-40G", 1.80),
        ("L40", 1.20),
        ("A10", 0.75),
        ("V100", 0.55),
        ("T4", 0.35),
    ];

    lines.push(ui::section("GPU Pricing (per GPU-hour)", color));
    let rows: Vec<Vec<Cell2>> = gpu_rates
        .iter()
        .map(|(gpu, rate)| {
            vec![
                (gpu.to_string(), None),
                (format!("${:.2}", rate), None),
                (format!("${:.2}", rate * 24.0), None),
                (format!("${:.0}", rate * 24.0 * 30.0), None),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &["GPU TYPE", "$/HOUR", "$/DAY", "$/MONTH"],
        &rows,
        color,
    ));
    lines.push(String::new());

    for quota in quotas {
        let quota_status = quota.status.clone().unwrap_or_default();
        let usage = &quota_status.current_usage;
        let limits = &quota.spec.gpu_quota;
        let kv = |key: &str, value: String| format!("    {}", ui::kv(key, &value, 20, color));

        lines.push(ui::section(&format!("Team: {}", quota.spec.team), color));
        lines.push(String::new());

        lines.push(format!("  {}", ui::ansi("Resource Usage", "1", color)));
        lines.push(kv("Allocated GPUs", usage.allocated_gpus.to_string()));
        lines.push(kv("Running Jobs", usage.running_jobs.to_string()));
        lines.push(kv("Queued Jobs", usage.queued_jobs.to_string()));
        lines.push(kv("GPU-Hours (MTD)", format!("{:.1}", usage.gpu_hours)));
        lines.push(String::new());

        lines.push(format!("  {}", ui::ansi("Quota Limits", "1", color)));
        lines.push(kv("Max GPUs", limits.max_gpus.to_string()));
        lines.push(kv("Max GPUs/Job", limits.max_gpus_per_job.to_string()));
        lines.push(kv("Max Running Jobs", limits.max_running_jobs.to_string()));
        if !limits.allowed_gpu_types.is_empty() {
            lines.push(kv("Allowed GPU Types", limits.allowed_gpu_types.join(", ")));
        }
        lines.push(String::new());

        if let Some(ref budget_spec) = quota.spec.budget {
            lines.push(format!("  {}", ui::ansi("Budget", "1", color)));
            lines.push(kv(
                "Monthly Budget",
                format!("${:.2}", budget_spec.monthly_budget),
            ));
            lines.push(kv(
                "Alert Threshold",
                format!("{:.0}%", budget_spec.alert_threshold * 100.0),
            ));
            lines.push(kv(
                "Hard Limit",
                if budget_spec.hard_limit {
                    Marker::Error.paint_with("yes", color)
                } else {
                    Marker::Ok.paint_with("no", color)
                },
            ));
            if let Some(ref bs) = quota_status.budget_status {
                lines.push(kv(
                    "Spent This Month",
                    format!("${:.2}", bs.spent_this_month),
                ));
                lines.push(kv("Remaining", format!("${:.2}", bs.remaining_budget)));
                lines.push(kv("Projected Spend", format!("${:.2}", bs.projected_spend)));
                if bs.projected_spend > budget_spec.monthly_budget {
                    lines.push(format!(
                        "    {} {}",
                        Marker::Error.paint_with(Marker::Error.glyph(), color),
                        Marker::Error.paint_with("Projected to exceed budget", color)
                    ));
                }
            }
            lines.push(String::new());
        }
    }
    lines
}

#[cfg(test)]
mod tests {
    use super::*;

    fn quota(team: &str, spent: f64, budget: f64, percent: f64, projected: f64) -> GryviaQuota {
        serde_json::from_value(serde_json::json!({
            "apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaQuota",
            "metadata": {"name": format!("{team}-quota")},
            "spec": {
                "team": team, "namespaces": [team],
                "gpuQuota": {"maxGPUs": 16, "maxGPUsPerJob": 8, "allowedGPUTypes": ["H100"], "maxRunningJobs": 4},
                "budget": {"monthlyBudget": budget, "alertThreshold": 0.8, "hardLimit": false}
            },
            "status": {
                "phase": "Active",
                "currentUsage": {"allocatedGPUs": 12, "runningJobs": 2, "queuedJobs": 1, "gpuHours": 100.0},
                "budgetStatus": {"spentThisMonth": spent, "remainingBudget": budget - spent, "percentUsed": percent, "projectedSpend": projected}
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
    fn cost_table_and_summary_aligned() {
        let a = quota("ml", 850.5, 1000.0, 85.05, 1100.0);
        let b = quota("research", 100.0, 2000.0, 5.0, 150.0);
        let text = cost_lines(&[&a, &b], "month", false).join("\n");
        let expected = "\
━━━ Cost Analysis (month) ━━━

TEAM      SPENT    BUDGET    REMAINING  % USED  PROJECTED
ml        $850.50  $1000.00  $149.50    85.0%   $1100.00
research  $100.00  $2000.00  $1900.00   5.0%    $150.00

Summary
  Total Spent      $950.50
  Total Budget     $3000.00
  Total Remaining  $2049.50
  Overall Usage    31.7%";
        assert_eq!(text, expected);
    }

    #[test]
    fn breakdown_lists_pricing_and_team_blocks() {
        let a = quota("ml", 850.5, 1000.0, 85.05, 1100.0);
        let text = breakdown_lines(&[&a], false).join("\n");
        assert!(text.starts_with("━━━ Detailed Breakdown ━━━\n\nGPU Pricing (per GPU-hour)\n"));
        assert!(text.contains("H100      $3.50   $84.00  $2520"));
        assert!(text.contains("Team: ml\n"));
        assert!(text.contains("    Max GPUs/Job        8\n"));
        assert!(text.contains("    Hard Limit          no\n"));
        assert!(text.contains("    ✗ Projected to exceed budget"));
    }

    #[test]
    fn color_output_strips_to_plain() {
        let a = quota("ml", 850.5, 1000.0, 85.05, 1100.0);
        let colored = cost_lines(&[&a], "week", true).join("\n");
        assert!(colored.contains("\x1b["));
        assert_eq!(strip(&colored), cost_lines(&[&a], "week", false).join("\n"));
        let colored = breakdown_lines(&[&a], true).join("\n");
        assert_eq!(strip(&colored), breakdown_lines(&[&a], false).join("\n"));
    }
}
