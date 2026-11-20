// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia maintenance`: cordon/annotate nodes for maintenance, optionally evict pods, and list nodes
//! currently marked. Pods are only ever removed through the Eviction API, which honours
//! PodDisruptionBudgets; a refused eviction is reported, never worked around.

use anyhow::{Context, Result};
use chrono::{DateTime, SecondsFormat, Utc};
use dialoguer::Confirm;
use k8s_openapi::api::core::v1::{Node, Pod};
use kube::api::{Api, EvictParams, ListParams, Patch, PatchParams};
use serde_json::json;

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

pub const ANN_START: &str = "gryvia.io/maintenance";
pub const ANN_REASON: &str = "gryvia.io/maintenance-reason";
const MIRROR_ANN: &str = "kubernetes.io/config.mirror";

// ---------- pure logic ----------

/// Merge-patch body for `start`: cordon and set the annotations. A node that is already in
/// maintenance keeps its original start time so re-running `start` does not reset the clock.
pub fn start_patch(
    now: DateTime<Utc>,
    reason: Option<&str>,
    existing_start: Option<&str>,
) -> serde_json::Value {
    let started = existing_start
        .map(str::to_string)
        .unwrap_or_else(|| now.to_rfc3339_opts(SecondsFormat::Secs, true));
    json!({
        "metadata": {"annotations": {
            ANN_START: started,
            ANN_REASON: reason.unwrap_or(""),
        }},
        "spec": {"unschedulable": true}
    })
}

/// Merge-patch body for `end`: uncordon and remove both annotations (null deletes a key).
pub fn end_patch() -> serde_json::Value {
    json!({
        "metadata": {"annotations": {ANN_START: null, ANN_REASON: null}},
        "spec": {"unschedulable": false}
    })
}

#[derive(Debug, PartialEq, Eq)]
pub enum SkipReason {
    DaemonSet,
    Mirror,
    Finished,
    Terminating,
}

impl SkipReason {
    pub fn label(&self) -> &'static str {
        match self {
            SkipReason::DaemonSet => "DaemonSet-managed",
            SkipReason::Mirror => "mirror (static) pod",
            SkipReason::Finished => "already finished",
            SkipReason::Terminating => "already terminating",
        }
    }
}

/// Why a pod must not be evicted during drain, or `None` if it should be.
pub fn skip_reason(pod: &Pod) -> Option<SkipReason> {
    if pod
        .metadata
        .annotations
        .as_ref()
        .is_some_and(|a| a.contains_key(MIRROR_ANN))
    {
        return Some(SkipReason::Mirror);
    }
    if pod
        .metadata
        .owner_references
        .as_ref()
        .is_some_and(|o| o.iter().any(|r| r.kind == "DaemonSet"))
    {
        return Some(SkipReason::DaemonSet);
    }
    if pod
        .status
        .as_ref()
        .and_then(|s| s.phase.as_deref())
        .is_some_and(|p| p == "Succeeded" || p == "Failed")
    {
        return Some(SkipReason::Finished);
    }
    if pod.metadata.deletion_timestamp.is_some() {
        return Some(SkipReason::Terminating);
    }
    None
}

#[derive(Debug, PartialEq, Eq)]
pub enum EvictOutcome {
    Evicted,
    /// 404: the pod was already gone.
    AlreadyGone,
    /// 429: refused, normally by a PodDisruptionBudget. The pod is left running.
    Blocked,
    Failed(String),
}

/// Classify an eviction API error by HTTP status code.
pub fn classify_eviction_error(code: u16, message: &str) -> EvictOutcome {
    match code {
        404 => EvictOutcome::AlreadyGone,
        429 => EvictOutcome::Blocked,
        _ => EvictOutcome::Failed(format!("{} (HTTP {})", message, code)),
    }
}

#[derive(Debug, Default, PartialEq, Eq)]
pub struct DrainSummary {
    pub evicted: Vec<String>,
    pub blocked: Vec<String>,
    pub failed: Vec<(String, String)>,
    pub skipped: Vec<(String, &'static str)>,
}

impl DrainSummary {
    pub fn record(&mut self, pod: &str, outcome: EvictOutcome) {
        match outcome {
            EvictOutcome::Evicted | EvictOutcome::AlreadyGone => self.evicted.push(pod.to_string()),
            EvictOutcome::Blocked => self.blocked.push(pod.to_string()),
            EvictOutcome::Failed(e) => self.failed.push((pod.to_string(), e)),
        }
    }

    /// True when every pod that should have been evicted was.
    pub fn is_complete(&self) -> bool {
        self.blocked.is_empty() && self.failed.is_empty()
    }

    pub fn render(&self) -> String {
        let mut s = format!(
            "Evicted {}, blocked {}, failed {}, skipped {}",
            self.evicted.len(),
            self.blocked.len(),
            self.failed.len(),
            self.skipped.len()
        );
        for p in &self.blocked {
            s.push_str(&format!(
                "\n  blocked  {} (eviction refused, usually by a PodDisruptionBudget; still running)",
                p
            ));
        }
        for (p, e) in &self.failed {
            s.push_str(&format!("\n  failed   {}: {}", p, e));
        }
        for (p, r) in &self.skipped {
            s.push_str(&format!("\n  skipped  {} ({})", p, r));
        }
        s
    }
}

/// Human-readable age like "3d", "5h", "12m", "40s".
pub fn format_duration(d: chrono::Duration) -> String {
    if d.num_seconds() < 0 {
        "clock skew".to_string()
    } else if d.num_days() > 0 {
        format!("{}d", d.num_days())
    } else if d.num_hours() > 0 {
        format!("{}h", d.num_hours())
    } else if d.num_minutes() > 0 {
        format!("{}m", d.num_minutes())
    } else {
        format!("{}s", d.num_seconds())
    }
}

#[derive(Debug, PartialEq, Eq, serde::Serialize)]
pub struct MaintenanceRow {
    pub node: String,
    pub cordoned: bool,
    pub age: String,
    pub since: String,
    pub reason: String,
}

/// Build a list row from a node, or `None` if it carries no maintenance annotation.
pub fn maintenance_row(node: &Node, now: DateTime<Utc>) -> Option<MaintenanceRow> {
    let ann = node.metadata.annotations.as_ref()?;
    let start = ann.get(ANN_START)?;
    let age = DateTime::parse_from_rfc3339(start)
        .map(|t| format_duration(now.signed_duration_since(t.with_timezone(&Utc))))
        .unwrap_or_else(|_| "unknown".to_string());
    Some(MaintenanceRow {
        node: node.metadata.name.clone().unwrap_or_default(),
        cordoned: node
            .spec
            .as_ref()
            .and_then(|s| s.unschedulable)
            .unwrap_or(false),
        age,
        since: start.clone(),
        reason: ann.get(ANN_REASON).cloned().unwrap_or_default(),
    })
}

// ---------- thin API layer ----------

pub async fn start(
    client: &GryviaClient,
    node: &str,
    reason: Option<&str>,
    drain: bool,
    yes: bool,
) -> Result<()> {
    let nodes: Api<Node> = Api::all(client.kube_client.clone());
    let existing = nodes
        .get(node)
        .await
        .with_context(|| format!("Failed to get node '{}'", node))?;

    if drain && !yes {
        let ok = Confirm::new()
            .with_prompt(format!(
                "Evict all non-DaemonSet pods from node '{}' (PodDisruptionBudgets are respected)?",
                node
            ))
            .interact()?;
        if !ok {
            display::print_info("Cancelled");
            return Ok(());
        }
    }

    let existing_start = existing
        .metadata
        .annotations
        .as_ref()
        .and_then(|a| a.get(ANN_START))
        .cloned();
    let patch = start_patch(Utc::now(), reason, existing_start.as_deref());
    nodes
        .patch(node, &PatchParams::default(), &Patch::Merge(&patch))
        .await
        .with_context(|| format!("Failed to cordon and annotate node '{}'", node))?;
    display::print_success(&format!(
        "Node {} cordoned and marked for maintenance",
        node
    ));

    if drain {
        let summary = drain_node(client, node).await?;
        println!("{}", summary.render());
        if !summary.is_complete() {
            anyhow::bail!(
                "Drain incomplete: {} blocked, {} failed. The node stays cordoned; retry \
                 `gryvia maintenance start {} --drain` once the blocking budgets allow it.",
                summary.blocked.len(),
                summary.failed.len(),
                node
            );
        }
        display::print_success(&format!("Node {} drained", node));
    }
    Ok(())
}

async fn drain_node(client: &GryviaClient, node: &str) -> Result<DrainSummary> {
    let all: Api<Pod> = Api::all(client.kube_client.clone());
    let pods = all
        .list(&ListParams::default().fields(&format!("spec.nodeName={}", node)))
        .await
        .context("Failed to list pods on node")?;

    let mut summary = DrainSummary::default();
    for pod in &pods.items {
        let name = pod.metadata.name.clone().unwrap_or_default();
        let ns = pod.metadata.namespace.clone().unwrap_or_default();
        let id = format!("{}/{}", ns, name);
        if let Some(reason) = skip_reason(pod) {
            summary.skipped.push((id, reason.label()));
            continue;
        }
        let api: Api<Pod> = Api::namespaced(client.kube_client.clone(), &ns);
        let outcome = match api.evict(&name, &EvictParams::default()).await {
            Ok(_) => EvictOutcome::Evicted,
            Err(kube::Error::Api(status)) => classify_eviction_error(status.code, &status.message),
            Err(e) => EvictOutcome::Failed(e.to_string()),
        };
        summary.record(&id, outcome);
    }
    Ok(summary)
}

pub async fn end(client: &GryviaClient, node: &str) -> Result<()> {
    let nodes: Api<Node> = Api::all(client.kube_client.clone());
    nodes
        .get(node)
        .await
        .with_context(|| format!("Failed to get node '{}'", node))?;
    nodes
        .patch(node, &PatchParams::default(), &Patch::Merge(&end_patch()))
        .await
        .with_context(|| format!("Failed to uncordon node '{}'", node))?;
    display::print_success(&format!(
        "Node {} uncordoned; maintenance annotations removed",
        node
    ));
    Ok(())
}

pub async fn list(client: &GryviaClient, output: &str) -> Result<()> {
    let nodes: Api<Node> = Api::all(client.kube_client.clone());
    let items = nodes
        .list(&ListParams::default())
        .await
        .context("Failed to list nodes")?;
    let now = Utc::now();
    let rows: Vec<MaintenanceRow> = items
        .items
        .iter()
        .filter_map(|n| maintenance_row(n, now))
        .collect();
    if output != "table" {
        return crate::output::print_serialized(output, &rows);
    }
    if rows.is_empty() {
        display::print_info("No nodes are marked for maintenance");
        return Ok(());
    }
    for line in maintenance_lines(&rows, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

/// The table of nodes marked for maintenance.
pub fn maintenance_lines(rows: &[MaintenanceRow], color: bool) -> Vec<String> {
    let cells: Vec<Vec<Cell2>> = rows
        .iter()
        .map(|r| {
            vec![
                (r.node.clone(), None),
                (
                    if r.cordoned { "yes" } else { "no" }.to_string(),
                    if r.cordoned { Some(Marker::Warn) } else { None },
                ),
                (r.age.clone(), None),
                (r.since.clone(), None),
                (r.reason.clone(), None),
            ]
        })
        .collect();
    ui::grid(
        &["NODE", "CORDONED", "AGE", "SINCE", "REASON"],
        &cells,
        color,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use k8s_openapi::api::core::v1::{NodeSpec, PodStatus};
    use k8s_openapi::apimachinery::pkg::apis::meta::v1::{ObjectMeta, OwnerReference, Time};
    use std::collections::BTreeMap;

    fn ts(s: &str) -> DateTime<Utc> {
        DateTime::parse_from_rfc3339(s).unwrap().with_timezone(&Utc)
    }

    fn owner(kind: &str) -> OwnerReference {
        OwnerReference {
            kind: kind.into(),
            api_version: "apps/v1".into(),
            name: "x".into(),
            uid: "u".into(),
            ..Default::default()
        }
    }

    fn pod(meta: ObjectMeta, phase: Option<&str>) -> Pod {
        Pod {
            metadata: meta,
            status: phase.map(|p| PodStatus {
                phase: Some(p.into()),
                ..Default::default()
            }),
            ..Default::default()
        }
    }

    #[test]
    fn start_patch_cordons_and_annotates() {
        let p = start_patch(ts("2026-09-29T10:00:00Z"), Some("firmware"), None);
        assert_eq!(p["spec"]["unschedulable"], true);
        assert_eq!(
            p["metadata"]["annotations"][ANN_START],
            "2026-09-29T10:00:00Z"
        );
        assert_eq!(p["metadata"]["annotations"][ANN_REASON], "firmware");
    }

    #[test]
    fn start_patch_keeps_existing_start_and_defaults_reason() {
        let p = start_patch(
            ts("2026-09-29T10:00:00Z"),
            None,
            Some("2026-09-01T00:00:00Z"),
        );
        assert_eq!(
            p["metadata"]["annotations"][ANN_START],
            "2026-09-01T00:00:00Z"
        );
        assert_eq!(p["metadata"]["annotations"][ANN_REASON], "");
    }

    #[test]
    fn end_patch_uncordons_and_nulls_annotations() {
        let p = end_patch();
        assert_eq!(p["spec"]["unschedulable"], false);
        assert!(p["metadata"]["annotations"][ANN_START].is_null());
        assert!(p["metadata"]["annotations"][ANN_REASON].is_null());
        assert!(p["metadata"]["annotations"]
            .as_object()
            .unwrap()
            .contains_key(ANN_START));
    }

    #[test]
    fn skips_daemonset_mirror_finished_terminating() {
        let ds = pod(
            ObjectMeta {
                owner_references: Some(vec![owner("DaemonSet")]),
                ..Default::default()
            },
            Some("Running"),
        );
        assert_eq!(skip_reason(&ds), Some(SkipReason::DaemonSet));

        let mut ann = BTreeMap::new();
        ann.insert(MIRROR_ANN.to_string(), "hash".to_string());
        let mirror = pod(
            ObjectMeta {
                annotations: Some(ann),
                ..Default::default()
            },
            Some("Running"),
        );
        assert_eq!(skip_reason(&mirror), Some(SkipReason::Mirror));

        assert_eq!(
            skip_reason(&pod(ObjectMeta::default(), Some("Succeeded"))),
            Some(SkipReason::Finished)
        );
        assert_eq!(
            skip_reason(&pod(ObjectMeta::default(), Some("Failed"))),
            Some(SkipReason::Finished)
        );

        let term = pod(
            ObjectMeta {
                deletion_timestamp: Some(Time(k8s_openapi::jiff::Timestamp::UNIX_EPOCH)),
                ..Default::default()
            },
            Some("Running"),
        );
        assert_eq!(skip_reason(&term), Some(SkipReason::Terminating));
    }

    #[test]
    fn evicts_ordinary_and_other_owned_pods() {
        assert_eq!(skip_reason(&pod(ObjectMeta::default(), None)), None);
        let rs = pod(
            ObjectMeta {
                owner_references: Some(vec![owner("ReplicaSet")]),
                ..Default::default()
            },
            Some("Running"),
        );
        assert_eq!(skip_reason(&rs), None);
        assert_eq!(
            skip_reason(&pod(ObjectMeta::default(), Some("Pending"))),
            None
        );
    }

    #[test]
    fn eviction_error_classification() {
        assert_eq!(
            classify_eviction_error(404, "gone"),
            EvictOutcome::AlreadyGone
        );
        assert_eq!(classify_eviction_error(429, "pdb"), EvictOutcome::Blocked);
        assert_eq!(
            classify_eviction_error(500, "boom"),
            EvictOutcome::Failed("boom (HTTP 500)".into())
        );
    }

    #[test]
    fn drain_summary_records_and_renders() {
        let mut s = DrainSummary::default();
        assert!(s.is_complete());
        s.record("a/one", EvictOutcome::Evicted);
        s.record("a/two", EvictOutcome::AlreadyGone);
        s.record("a/three", EvictOutcome::Blocked);
        s.record("a/four", EvictOutcome::Failed("x".into()));
        s.skipped
            .push(("kube-system/ds".into(), SkipReason::DaemonSet.label()));
        assert_eq!(s.evicted.len(), 2);
        assert!(!s.is_complete());
        let out = s.render();
        assert!(out.starts_with("Evicted 2, blocked 1, failed 1, skipped 1"));
        assert!(out.contains("blocked  a/three"));
        assert!(out.contains("failed   a/four: x"));
        assert!(out.contains("skipped  kube-system/ds (DaemonSet-managed)"));
    }

    #[test]
    fn formats_durations() {
        use chrono::Duration;
        assert_eq!(format_duration(Duration::seconds(40)), "40s");
        assert_eq!(format_duration(Duration::minutes(12)), "12m");
        assert_eq!(format_duration(Duration::hours(5)), "5h");
        assert_eq!(format_duration(Duration::days(3)), "3d");
        assert_eq!(format_duration(Duration::seconds(-5)), "clock skew");
    }

    #[test]
    fn maintenance_row_only_for_annotated_nodes() {
        let now = ts("2026-09-29T12:00:00Z");
        let plain = Node {
            metadata: ObjectMeta {
                name: Some("n0".into()),
                ..Default::default()
            },
            ..Default::default()
        };
        assert!(maintenance_row(&plain, now).is_none());

        let mut ann = BTreeMap::new();
        ann.insert(ANN_START.to_string(), "2026-09-29T09:00:00Z".to_string());
        ann.insert(ANN_REASON.to_string(), "psu swap".to_string());
        let node = Node {
            metadata: ObjectMeta {
                name: Some("n1".into()),
                annotations: Some(ann),
                ..Default::default()
            },
            spec: Some(NodeSpec {
                unschedulable: Some(true),
                ..Default::default()
            }),
            ..Default::default()
        };
        let row = maintenance_row(&node, now).unwrap();
        assert_eq!(
            row,
            MaintenanceRow {
                node: "n1".into(),
                cordoned: true,
                age: "3h".into(),
                since: "2026-09-29T09:00:00Z".into(),
                reason: "psu swap".into()
            }
        );
    }

    #[test]
    fn maintenance_row_tolerates_bad_timestamp_and_missing_reason() {
        let mut ann = BTreeMap::new();
        ann.insert(ANN_START.to_string(), "not-a-time".to_string());
        let node = Node {
            metadata: ObjectMeta {
                name: Some("n".into()),
                annotations: Some(ann),
                ..Default::default()
            },
            ..Default::default()
        };
        let row = maintenance_row(&node, ts("2026-09-29T12:00:00Z")).unwrap();
        assert_eq!(row.age, "unknown");
        assert_eq!(row.reason, "");
        assert!(!row.cordoned);
    }

    #[test]
    fn maintenance_table_aligned() {
        let rows = vec![
            MaintenanceRow {
                node: "gpu-node-1".into(),
                cordoned: true,
                age: "3h".into(),
                since: "2026-09-29T09:00:00Z".into(),
                reason: "psu swap".into(),
            },
            MaintenanceRow {
                node: "n2".into(),
                cordoned: false,
                age: "2d".into(),
                since: "2026-09-27T09:00:00Z".into(),
                reason: String::new(),
            },
        ];
        assert_eq!(
            maintenance_lines(&rows, false),
            vec![
                "NODE        CORDONED  AGE  SINCE                 REASON",
                "gpu-node-1  yes       3h   2026-09-29T09:00:00Z  psu swap",
                "n2          no        2d   2026-09-27T09:00:00Z",
            ]
        );
        let colored = maintenance_lines(&rows, true);
        assert!(colored[1].contains("\x1b[33myes"));
        assert!(colored[0].contains("\x1b["));
    }
}
