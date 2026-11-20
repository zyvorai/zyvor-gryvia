// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia capacity`: read-only GPU capacity report computed from cluster objects.
//!
//! Supply comes from GryviaGpuNode (`spec.gpuType`, `spec.gpuCount`). Allocation comes from running
//! GryviaAIJobs, demand from pending ones. Phase grouping matches the gateway
//! (`services/api-gateway/routers/phases.py`): case-insensitive, Scheduling/Queued/Pending are pending
//! and Succeeded/Completed are completed. Nothing is forecast; every number is a count of GPUs
//! currently registered or requested.

use std::collections::BTreeMap;

use anyhow::{Context, Result};
use kube::api::{Api, ApiResource, GroupVersionKind, ListParams};
use kube::core::DynamicObject;
use serde::Serialize;

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

/// A GPU node as far as capacity is concerned.
#[derive(Clone, Debug, PartialEq)]
pub struct NodeInfo {
    pub gpu_type: String,
    pub gpu_count: u32,
}

/// A job as far as capacity is concerned. `gpu_type` is `None` when the job did not name a type
/// (missing, empty or `any`), meaning it can run on any GPU type.
#[derive(Clone, Debug, PartialEq)]
pub struct JobInfo {
    pub phase: String,
    pub gpus: u32,
    pub gpu_type: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct TypeCapacity {
    pub gpu_type: String,
    pub total: u32,
    pub allocated: u32,
    /// `total - allocated`, floored at 0 (see `oversubscribed_by`).
    pub free: u32,
    /// GPUs requested by pending jobs that name this type.
    pub pending_demand: u32,
    /// Pending demand that cannot be met from free GPUs of this type.
    pub shortfall: u32,
    /// Free GPUs left over after pending demand for this type.
    pub headroom: u32,
    /// Running jobs hold more GPUs of this type than nodes provide (e.g. a node was removed).
    pub oversubscribed_by: u32,
    /// True when no node registers this type at all, only jobs mention it.
    pub unknown_type: bool,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct Unspecified {
    /// GPUs held by running jobs that did not name a type.
    pub allocated: u32,
    /// GPUs requested by pending jobs that did not name a type.
    pub pending_demand: u32,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct CapacityReport {
    pub types: Vec<TypeCapacity>,
    /// Jobs without a GPU type. Not attributable to a single type, so kept out of `types`.
    /// Always zero when a `--gpu-type` filter is in effect (those jobs are excluded).
    pub unspecified: Unspecified,
    pub total: u32,
    pub allocated: u32,
    pub free: u32,
    pub pending_demand: u32,
    /// Pending demand (typed and untyped) that exceeds cluster-wide free GPUs.
    pub shortfall: u32,
    pub headroom: u32,
}

/// Phase bucket, mirroring the gateway's `phases.bucket`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PhaseBucket {
    Running,
    Pending,
    Completed,
    Failed,
    Other,
}

pub fn phase_bucket(phase: &str) -> PhaseBucket {
    match phase.trim().to_ascii_lowercase().as_str() {
        "running" => PhaseBucket::Running,
        "scheduling" | "queued" | "pending" => PhaseBucket::Pending,
        "succeeded" | "completed" => PhaseBucket::Completed,
        "failed" => PhaseBucket::Failed,
        _ => PhaseBucket::Other,
    }
}

/// Normalise a job's GPU type: empty and `any` mean "unspecified".
pub fn normalize_job_gpu_type(raw: Option<&str>) -> Option<String> {
    let t = raw?.trim();
    if t.is_empty() || t.eq_ignore_ascii_case("any") {
        None
    } else {
        Some(t.to_string())
    }
}

/// Pure capacity computation. `gpu_type_filter` matches case-insensitively; when set, only nodes and
/// jobs of that type are considered and untyped jobs are left out.
pub fn compute_capacity(
    nodes: &[NodeInfo],
    jobs: &[JobInfo],
    gpu_type_filter: Option<&str>,
) -> CapacityReport {
    struct Acc {
        display: String,
        total: u32,
        allocated: u32,
        pending: u32,
        known: bool,
    }
    let mut by_type: BTreeMap<String, Acc> = BTreeMap::new();
    let mut unspecified = Unspecified::default();
    let filter = gpu_type_filter.map(|f| f.trim().to_ascii_lowercase());
    let entry = |m: &mut BTreeMap<String, Acc>, t: &str| -> String {
        let key = t.trim().to_ascii_lowercase();
        m.entry(key.clone()).or_insert_with(|| Acc {
            display: t.trim().to_string(),
            total: 0,
            allocated: 0,
            pending: 0,
            known: false,
        });
        key
    };
    let wanted = |t: &str| {
        filter
            .as_ref()
            .is_none_or(|f| *f == t.trim().to_ascii_lowercase())
    };

    for n in nodes {
        if n.gpu_type.trim().is_empty() || !wanted(&n.gpu_type) {
            continue;
        }
        let k = entry(&mut by_type, &n.gpu_type);
        let a = by_type.get_mut(&k).unwrap();
        a.total = a.total.saturating_add(n.gpu_count);
        a.known = true;
    }

    for j in jobs {
        let bucket = phase_bucket(&j.phase);
        if bucket != PhaseBucket::Running && bucket != PhaseBucket::Pending {
            continue;
        }
        match normalize_job_gpu_type(j.gpu_type.as_deref()) {
            Some(t) => {
                if !wanted(&t) {
                    continue;
                }
                let k = entry(&mut by_type, &t);
                let a = by_type.get_mut(&k).unwrap();
                if bucket == PhaseBucket::Running {
                    a.allocated = a.allocated.saturating_add(j.gpus);
                } else {
                    a.pending = a.pending.saturating_add(j.gpus);
                }
            }
            None if filter.is_none() => {
                if bucket == PhaseBucket::Running {
                    unspecified.allocated = unspecified.allocated.saturating_add(j.gpus);
                } else {
                    unspecified.pending_demand = unspecified.pending_demand.saturating_add(j.gpus);
                }
            }
            None => {}
        }
    }

    let types: Vec<TypeCapacity> = by_type
        .into_values()
        .map(|a| {
            let free = a.total.saturating_sub(a.allocated);
            TypeCapacity {
                gpu_type: a.display,
                total: a.total,
                allocated: a.allocated,
                free,
                pending_demand: a.pending,
                shortfall: a.pending.saturating_sub(free),
                headroom: free.saturating_sub(a.pending),
                oversubscribed_by: a.allocated.saturating_sub(a.total),
                unknown_type: !a.known,
            }
        })
        .collect();

    let total: u32 = types.iter().map(|t| t.total).sum();
    let allocated: u32 = types.iter().map(|t| t.allocated).sum::<u32>() + unspecified.allocated;
    let pending_demand: u32 =
        types.iter().map(|t| t.pending_demand).sum::<u32>() + unspecified.pending_demand;
    let free = total.saturating_sub(allocated);
    CapacityReport {
        types,
        unspecified,
        total,
        allocated,
        free,
        pending_demand,
        shortfall: pending_demand.saturating_sub(free),
        headroom: free.saturating_sub(pending_demand),
    }
}

pub(crate) fn gvk_api(client: &GryviaClient, kind: &str, plural: &str) -> Api<DynamicObject> {
    let gvk = GroupVersionKind::gvk("gryvia.io", "v1alpha1", kind);
    let ar = ApiResource::from_gvk_with_plural(&gvk, plural);
    Api::all_with(client.kube_client.clone(), &ar)
}

fn u32_at(v: &serde_json::Value, ptr: &str) -> Option<u32> {
    v.pointer(ptr)?
        .as_u64()
        .map(|n| n.min(u32::MAX as u64) as u32)
}

/// Read a node object from its raw JSON (the CRD only requires gpuType and gpuCount).
pub fn node_from_json(v: &serde_json::Value) -> Option<NodeInfo> {
    Some(NodeInfo {
        gpu_type: v.pointer("/spec/gpuType")?.as_str()?.to_string(),
        gpu_count: u32_at(v, "/spec/gpuCount")?,
    })
}

/// Read a job from its raw JSON. Running jobs use `status.gpusAllocated` when the operator set it,
/// otherwise `spec.gpus`.
pub fn job_from_json(v: &serde_json::Value) -> JobInfo {
    let phase = v
        .pointer("/status/phase")
        .and_then(|p| p.as_str())
        .unwrap_or("")
        .to_string();
    let spec_gpus = u32_at(v, "/spec/gpus").unwrap_or(0);
    let gpus = match (phase_bucket(&phase), u32_at(v, "/status/gpusAllocated")) {
        (PhaseBucket::Running, Some(n)) if n > 0 => n,
        _ => spec_gpus,
    };
    JobInfo {
        phase,
        gpus,
        gpu_type: v
            .pointer("/spec/gpuType")
            .and_then(|t| t.as_str())
            .map(str::to_string),
    }
}

pub async fn execute(client: &GryviaClient, output: &str, gpu_type: Option<&str>) -> Result<()> {
    let nodes_api = gvk_api(client, "GryviaGpuNode", "gryviagpunodes");
    let jobs_api = gvk_api(client, "GryviaAIJob", "gryviaaijobs");
    let nodes = nodes_api
        .list(&ListParams::default())
        .await
        .context("Failed to list GPU nodes")?;
    let jobs = jobs_api
        .list(&ListParams::default())
        .await
        .context("Failed to list jobs")?;

    let node_infos: Vec<NodeInfo> = nodes
        .items
        .iter()
        .filter_map(|o| node_from_json(&to_value(o)))
        .collect();
    let job_infos: Vec<JobInfo> = jobs
        .items
        .iter()
        .map(|o| job_from_json(&to_value(o)))
        .collect();

    let report = compute_capacity(&node_infos, &job_infos, gpu_type);
    if output == "json" || output == "yaml" {
        crate::output::print_serialized(output, &report)?;
    } else {
        print_report(&report, gpu_type);
    }
    Ok(())
}

fn to_value(o: &DynamicObject) -> serde_json::Value {
    serde_json::to_value(o).unwrap_or(serde_json::Value::Null)
}

fn print_report(r: &CapacityReport, filter: Option<&str>) {
    let lines = capacity_lines(r, filter, ui::color_enabled());
    for line in &lines {
        println!("{line}");
    }
    if r.types.is_empty() && r.unspecified == Unspecified::default() {
        display::print_warning("No GPU nodes or active jobs found");
    }
}

/// The human-readable capacity report. When there is nothing to show it is only the header; the caller
/// prints the "nothing found" warning.
pub fn capacity_lines(r: &CapacityReport, filter: Option<&str>, color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("GPU Capacity", color), String::new()];
    if r.types.is_empty() && r.unspecified == Unspecified::default() {
        return lines;
    }

    let rows: Vec<Vec<Cell2>> = r
        .types
        .iter()
        .map(|t| {
            let (note, note_marker) = if t.unknown_type {
                (
                    "no node registers this type".to_string(),
                    Some(Marker::Warn),
                )
            } else if t.oversubscribed_by > 0 {
                (
                    format!("allocated exceeds total by {}", t.oversubscribed_by),
                    Some(Marker::Warn),
                )
            } else {
                (String::new(), None)
            };
            vec![
                (t.gpu_type.clone(), None),
                (t.total.to_string(), None),
                (t.allocated.to_string(), None),
                (t.free.to_string(), None),
                (t.pending_demand.to_string(), None),
                (
                    t.shortfall.to_string(),
                    if t.shortfall > 0 {
                        Some(Marker::Error)
                    } else {
                        None
                    },
                ),
                (t.headroom.to_string(), None),
                (note, note_marker),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &[
            "GPU TYPE",
            "TOTAL",
            "ALLOCATED",
            "FREE",
            "PENDING",
            "SHORTFALL",
            "HEADROOM",
            "NOTE",
        ],
        &rows,
        color,
    ));
    lines.push(String::new());
    lines.push(format!(
        "  Total {}  Allocated {}  Free {}  Pending demand {}",
        r.total, r.allocated, r.free, r.pending_demand
    ));
    if filter.is_none() {
        lines.push(format!(
            "  Jobs without a GPU type: {} allocated, {} pending (counted only in the totals above)",
            r.unspecified.allocated, r.unspecified.pending_demand
        ));
    } else {
        lines.push("  Jobs without a GPU type are excluded while --gpu-type is set".to_string());
    }
    if r.shortfall > 0 {
        lines.push(format!(
            "  {} pending demand exceeds free GPUs by {}",
            Marker::Error.paint_with("Shortfall:", color),
            r.shortfall
        ));
    } else {
        lines.push(format!(
            "  {} {} GPUs after pending demand",
            Marker::Ok.paint_with("Headroom:", color),
            r.headroom
        ));
    }
    lines
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn node(t: &str, c: u32) -> NodeInfo {
        NodeInfo {
            gpu_type: t.into(),
            gpu_count: c,
        }
    }
    fn job(phase: &str, gpus: u32, t: Option<&str>) -> JobInfo {
        JobInfo {
            phase: phase.into(),
            gpus,
            gpu_type: t.map(str::to_string),
        }
    }
    fn row<'a>(r: &'a CapacityReport, t: &str) -> &'a TypeCapacity {
        r.types.iter().find(|x| x.gpu_type == t).unwrap()
    }

    #[test]
    fn empty_cluster() {
        let r = compute_capacity(&[], &[], None);
        assert_eq!(r, CapacityReport::default());
    }

    #[test]
    fn totals_and_free_per_type() {
        let nodes = [node("H100", 8), node("H100", 8), node("A100", 4)];
        let jobs = [
            job("Running", 6, Some("H100")),
            job("Completed", 8, Some("A100")),
        ];
        let r = compute_capacity(&nodes, &jobs, None);
        let h = row(&r, "H100");
        assert_eq!((h.total, h.allocated, h.free), (16, 6, 10));
        let a = row(&r, "A100");
        assert_eq!((a.total, a.allocated, a.free), (4, 0, 4));
        assert_eq!((r.total, r.allocated, r.free), (20, 6, 14));
    }

    #[test]
    fn pending_phases_use_gateway_grouping() {
        let nodes = [node("H100", 8)];
        let jobs = [
            job("Pending", 1, Some("H100")),
            job("queued", 2, Some("H100")),
            job(" Scheduling ", 4, Some("H100")),
            job("Succeeded", 8, Some("H100")),
            job("Failed", 8, Some("H100")),
            job("", 8, Some("H100")),
        ];
        let r = compute_capacity(&nodes, &jobs, None);
        assert_eq!(row(&r, "H100").pending_demand, 7);
        assert_eq!(row(&r, "H100").allocated, 0);
    }

    #[test]
    fn shortfall_and_headroom() {
        let nodes = [node("H100", 8), node("A100", 8)];
        let jobs = [
            job("Running", 6, Some("H100")),
            job("Queued", 5, Some("H100")),
            job("Queued", 3, Some("A100")),
        ];
        let r = compute_capacity(&nodes, &jobs, None);
        let h = row(&r, "H100");
        assert_eq!((h.free, h.shortfall, h.headroom), (2, 3, 0));
        let a = row(&r, "A100");
        assert_eq!((a.free, a.shortfall, a.headroom), (8, 0, 5));
        assert_eq!((r.shortfall, r.headroom), (0, 2));
    }

    #[test]
    fn oversubscription_is_reported_not_wrapped() {
        let nodes = [node("H100", 4)];
        let jobs = [
            job("Running", 6, Some("H100")),
            job("Pending", 2, Some("H100")),
        ];
        let r = compute_capacity(&nodes, &jobs, None);
        let h = row(&r, "H100");
        assert_eq!(
            (h.free, h.oversubscribed_by, h.shortfall, h.headroom),
            (0, 2, 2, 0)
        );
        assert_eq!((r.free, r.shortfall), (0, 2));
    }

    #[test]
    fn unknown_gpu_type_gets_zero_supply_row() {
        let nodes = [node("H100", 8)];
        let jobs = [job("Pending", 2, Some("B200"))];
        let r = compute_capacity(&nodes, &jobs, None);
        let b = row(&r, "B200");
        assert!(b.unknown_type);
        assert_eq!((b.total, b.pending_demand, b.shortfall), (0, 2, 2));
        assert!(!row(&r, "H100").unknown_type);
        // Cluster-wide, the unknown type's demand can still be absorbed by free H100s.
        assert_eq!((r.pending_demand, r.shortfall, r.headroom), (2, 0, 6));
    }

    #[test]
    fn jobs_without_type_count_only_in_totals() {
        let nodes = [node("H100", 8), node("A100", 4)];
        let jobs = [
            job("Running", 3, None),
            job("Running", 1, Some("any")),
            job("Pending", 10, Some("")),
            job("Pending", 1, Some("ANY")),
        ];
        let r = compute_capacity(&nodes, &jobs, None);
        assert_eq!(
            r.unspecified,
            Unspecified {
                allocated: 4,
                pending_demand: 11
            }
        );
        assert!(r
            .types
            .iter()
            .all(|t| t.allocated == 0 && t.pending_demand == 0));
        assert_eq!((r.total, r.allocated, r.free), (12, 4, 8));
        assert_eq!((r.pending_demand, r.shortfall, r.headroom), (11, 3, 0));
    }

    #[test]
    fn type_matching_is_case_insensitive() {
        let nodes = [node("h100", 8)];
        let jobs = [job("Running", 2, Some("H100"))];
        let r = compute_capacity(&nodes, &jobs, None);
        assert_eq!(r.types.len(), 1);
        assert_eq!(r.types[0].allocated, 2);
    }

    #[test]
    fn gpu_type_filter_limits_rows_and_excludes_untyped() {
        let nodes = [node("H100", 8), node("A100", 4)];
        let jobs = [
            job("Running", 2, Some("A100")),
            job("Pending", 5, None),
            job("Pending", 1, Some("H100")),
        ];
        let r = compute_capacity(&nodes, &jobs, Some("a100"));
        assert_eq!(r.types.len(), 1);
        assert_eq!(
            (r.total, r.allocated, r.free, r.pending_demand),
            (4, 2, 2, 0)
        );
        assert_eq!(r.unspecified, Unspecified::default());
    }

    #[test]
    fn filter_with_no_match_is_empty() {
        let r = compute_capacity(&[node("H100", 8)], &[], Some("T4"));
        assert!(r.types.is_empty());
        assert_eq!(r.total, 0);
    }

    #[test]
    fn node_and_job_json_parsing() {
        let n = node_from_json(&json!({"spec": {"gpuType": "H100", "gpuCount": 8}})).unwrap();
        assert_eq!(n, node("H100", 8));
        assert!(node_from_json(&json!({"spec": {"gpuCount": 8}})).is_none());

        let j = job_from_json(&json!({
            "spec": {"gpus": 4, "gpuType": "A100"},
            "status": {"phase": "Running", "gpusAllocated": 2}
        }));
        assert_eq!(j, job("Running", 2, Some("A100")));
        let j = job_from_json(
            &json!({"spec": {"gpus": 4}, "status": {"phase": "Queued", "gpusAllocated": 2}}),
        );
        assert_eq!(j, job("Queued", 4, None));
        let j = job_from_json(&json!({"spec": {"gpus": 4}}));
        assert_eq!(j.phase, "");
        assert_eq!(j.gpus, 4);
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

    fn sample_report() -> CapacityReport {
        compute_capacity(
            &[node("H100", 16), node("A100", 8)],
            &[
                job("Running", 12, Some("H100")),
                job("Pending", 8, Some("H100")),
                job("Running", 2, None),
            ],
            None,
        )
    }

    #[test]
    fn capacity_report_aligned() {
        let text = capacity_lines(&sample_report(), None, false).join("\n");
        let expected = "\
━━━ GPU Capacity ━━━

GPU TYPE  TOTAL  ALLOCATED  FREE  PENDING  SHORTFALL  HEADROOM  NOTE
A100      8      0          8     0        0          8
H100      16     12         4     8        4          0

  Total 24  Allocated 14  Free 10  Pending demand 8
  Jobs without a GPU type: 2 allocated, 0 pending (counted only in the totals above)
  Headroom: 2 GPUs after pending demand";
        assert_eq!(text, expected);
    }

    #[test]
    fn capacity_notes_and_empty_report() {
        let r = compute_capacity(&[], &[job("Running", 4, Some("T4"))], Some("T4"));
        let text = capacity_lines(&r, Some("T4"), false).join("\n");
        assert!(text.contains("no node registers this type"));
        assert!(text.contains("excluded while --gpu-type is set"));
        assert!(
            text.contains("Shortfall: pending demand exceeds free GPUs by 0")
                || text.contains("Headroom:")
        );
        assert_eq!(
            capacity_lines(&CapacityReport::default(), None, false),
            vec!["━━━ GPU Capacity ━━━", ""]
        );
    }

    #[test]
    fn capacity_color_strips_to_plain() {
        let r = sample_report();
        let colored = capacity_lines(&r, None, true).join("\n");
        assert!(colored.contains("\x1b["));
        assert_eq!(strip(&colored), capacity_lines(&r, None, false).join("\n"));
    }
}
