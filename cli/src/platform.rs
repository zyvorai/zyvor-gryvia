//! Platform status, in the style of `cilium status`: one line per component, workload readiness, cluster
//! totals and a per-node report.
//!
//! Everything here is pure: [`Inputs`] (plain structs read from the cluster by `commands::platform_status`)
//! go in, a [`StatusModel`] comes out, and [`render`] turns it into text. That keeps the whole feature
//! testable without a cluster.

use std::collections::BTreeMap;

use serde::Serialize;

use crate::ui::{self, Cell2, Marker};

// ── Inputs ───────────────────────────────────────────────────────────────────────────────────────

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
pub enum WorkloadKind {
    Deployment,
    DaemonSet,
}

impl WorkloadKind {
    fn label(self) -> &'static str {
        match self {
            WorkloadKind::Deployment => "Deployment",
            WorkloadKind::DaemonSet => "DaemonSet",
        }
    }
}

#[derive(Clone, Debug, Serialize)]
pub struct WorkloadInfo {
    pub kind: WorkloadKind,
    pub name: String,
    pub desired: i32,
    pub ready: i32,
    pub available: i32,
    pub images: Vec<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct PodInfo {
    pub name: String,
    pub node: Option<String>,
    pub phase: String,
    pub ready: bool,
    /// Why it is not ready (for example `ImagePullBackOff`), when Kubernetes says so.
    pub reason: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct NodeInfo {
    pub name: String,
    pub ready: bool,
    pub unschedulable: bool,
    /// Value of the `gryvia.io/maintenance-reason` annotation, or `maintenance` when only the start time is set.
    pub maintenance: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct GpuNodeInfo {
    /// The Kubernetes node this GryviaGpuNode describes (`spec.nodeName`).
    pub node_name: String,
    pub gpu_type: String,
    pub gpu_count: u32,
    /// `status.phase` (Ready, Initializing, Failed, ...).
    pub phase: String,
    pub driver: String,
    pub cuda: String,
    /// Entries in `status.gpuStatus` and how many of them report `Healthy`.
    pub gpus_reported: u32,
    pub gpus_healthy: u32,
}

#[derive(Clone, Debug, Default, Serialize, PartialEq, Eq)]
pub struct JobCounts {
    pub running: u32,
    pub pending: u32,
    pub completed: u32,
    pub failed: u32,
}

#[derive(Clone, Debug, Default)]
pub struct Inputs {
    pub namespace: String,
    pub workloads: Vec<WorkloadInfo>,
    pub pods: Vec<PodInfo>,
    pub nodes: Vec<NodeInfo>,
    pub gpu_nodes: Vec<GpuNodeInfo>,
    pub jobs: JobCounts,
    /// Problems reading the cluster (for example a forbidden list). Shown as warnings.
    pub warnings: Vec<String>,
    /// The GryviaGpuNode or job list could not be read, so an empty list means "unknown", not "none".
    pub gpu_nodes_failed: bool,
    pub jobs_failed: bool,
    /// Detected from ai-operator Deployment args: kai | kueue | native | volcano | default.
    pub scheduler_backend: String,
}

// ── Model ────────────────────────────────────────────────────────────────────────────────────────

#[derive(Clone, Debug, Serialize)]
pub struct ComponentLine {
    pub name: String,
    pub state: Marker,
    pub detail: String,
    /// Required components decide the overall state and the exit code.
    pub required: bool,
}

#[derive(Clone, Debug, Serialize)]
pub struct WorkloadLine {
    pub kind: WorkloadKind,
    pub name: String,
    pub desired: i32,
    pub ready: i32,
    pub available: i32,
    pub state: Marker,
}

/// The DaemonSet pods every GPU node is expected to run.
pub const NODE_ADDONS: &[(&str, &str)] = &[
    ("dcgm", "dcgm-exporter"),
    ("plugin", "nvidia-device-plugin"),
    ("collector", "collector"),
];

#[derive(Clone, Debug, Serialize)]
pub struct NodeRow {
    pub name: String,
    pub ready: Marker,
    pub ready_text: String,
    pub gpus: String,
    pub driver: String,
    pub gpu_health: Marker,
    pub gpu_health_text: String,
    pub maintenance: Option<String>,
    /// `dcgm`, `plugin`, `collector`: Ok = running, Error = present but not ready, Disabled = no pod.
    pub addons: BTreeMap<String, Marker>,
    pub pods: usize,
}

#[derive(Clone, Debug, Serialize)]
pub struct StatusModel {
    pub namespace: String,
    pub overall: Marker,
    pub components: Vec<ComponentLine>,
    pub workloads: Vec<WorkloadLine>,
    pub pods_running: usize,
    pub pods_total: usize,
    /// `None` when the jobs could not be listed.
    pub jobs: Option<JobCounts>,
    pub images: Vec<(String, String)>,
    pub nodes: Vec<NodeRow>,
    pub errors: Vec<String>,
    pub warnings: Vec<String>,
}

impl StatusModel {
    /// True when a required component is in Error: the exit code and `--brief` result.
    pub fn failed(&self) -> bool {
        self.overall == Marker::Error
    }

    /// Keep only the node rows for `name` (the `--node` flag).
    pub fn only_node(&mut self, name: &str) {
        self.nodes.retain(|n| n.name == name);
    }
}

fn workload_state(w: &WorkloadInfo) -> Marker {
    if w.desired == 0 {
        Marker::Disabled
    } else if w.ready >= w.desired {
        Marker::Ok
    } else if w.ready == 0 {
        Marker::Error
    } else {
        Marker::Warn
    }
}

/// Worst state of a group; a group with nothing running is `Disabled`.
fn roll_up(states: &[Marker]) -> Marker {
    if states.is_empty() {
        return Marker::Disabled;
    }
    states
        .iter()
        .copied()
        .fold(Marker::Disabled, |acc, m| acc.worst(m))
}

/// Short operator name: `gryvia-gpu-operator` becomes `gpu`.
fn operator_short_name(name: &str) -> String {
    name.trim_start_matches("gryvia-")
        .trim_end_matches("-operator")
        .to_string()
}

fn is_operator(name: &str) -> bool {
    name.ends_with("-operator")
}

fn is_gateway(name: &str) -> bool {
    name.contains("api-gateway")
}

fn is_dashboard(name: &str) -> bool {
    name.ends_with("gryvia-ui") || name.ends_with("-ui")
}

fn is_collector(name: &str) -> bool {
    name.contains("collector")
}

fn is_gpu_addon(name: &str) -> bool {
    name.contains("dcgm-exporter") || name.contains("nvidia-device-plugin")
}

fn pod_ok(p: &PodInfo) -> bool {
    (p.phase == "Running" && p.ready) || p.phase == "Succeeded"
}

/// Build the model from what was read from the cluster.
pub fn build_model(inputs: &Inputs) -> StatusModel {
    let workloads: Vec<WorkloadLine> = inputs
        .workloads
        .iter()
        .map(|w| WorkloadLine {
            kind: w.kind,
            name: w.name.clone(),
            desired: w.desired,
            ready: w.ready,
            available: w.available,
            state: workload_state(w),
        })
        .collect();

    let states_of = |pred: &dyn Fn(&str) -> bool| -> (Vec<Marker>, Vec<&WorkloadLine>) {
        let lines: Vec<&WorkloadLine> = workloads.iter().filter(|w| pred(&w.name)).collect();
        (lines.iter().map(|w| w.state).collect(), lines)
    };

    let mut components: Vec<ComponentLine> = Vec::new();

    // Operators (required)
    let (op_states, op_lines) = states_of(&is_operator);
    let operators_state = if op_lines.is_empty() {
        Marker::Error
    } else {
        roll_up(&op_states)
    };
    let operators_detail = if op_lines.is_empty() {
        "not installed".to_string()
    } else {
        op_lines
            .iter()
            .map(|w| operator_short_name(&w.name))
            .collect::<Vec<_>>()
            .join(", ")
    };
    components.push(ComponentLine {
        name: "Operators".into(),
        state: operators_state,
        detail: operators_detail,
        required: true,
    });

    // API gateway and dashboard (required)
    for (label, pred) in [
        ("API gateway", is_gateway as fn(&str) -> bool),
        ("Dashboard", is_dashboard as fn(&str) -> bool),
    ] {
        let (states, lines) = states_of(&pred);
        let (state, detail) = if lines.is_empty() {
            (Marker::Error, "not installed".to_string())
        } else {
            let w = lines[0];
            (roll_up(&states), format!("{}/{} ready", w.ready, w.desired))
        };
        components.push(ComponentLine {
            name: label.into(),
            state,
            detail,
            required: true,
        });
    }

    // GPU nodes (optional): registered GryviaGpuNodes and their phase
    let ready_gpu_nodes = inputs
        .gpu_nodes
        .iter()
        .filter(|g| matches!(g.phase.as_str(), "Ready" | "Healthy" | "Active"))
        .count();
    let (gpu_state, gpu_detail) = if inputs.gpu_nodes_failed {
        (Marker::Unknown, "unavailable".to_string())
    } else if inputs.gpu_nodes.is_empty() {
        (Marker::Disabled, "none registered".to_string())
    } else if ready_gpu_nodes == inputs.gpu_nodes.len() {
        (
            Marker::Ok,
            format!("{}/{} ready", ready_gpu_nodes, inputs.gpu_nodes.len()),
        )
    } else {
        (
            Marker::Warn,
            format!("{}/{} ready", ready_gpu_nodes, inputs.gpu_nodes.len()),
        )
    };
    components.push(ComponentLine {
        name: "GPU nodes".into(),
        state: gpu_state,
        detail: gpu_detail,
        required: false,
    });

    // GPU add-ons (optional): DCGM exporter and NVIDIA device plugin
    let (addon_states, addon_lines) = states_of(&is_gpu_addon);
    components.push(ComponentLine {
        name: "GPU add-ons".into(),
        state: roll_up(&addon_states),
        detail: if addon_lines.is_empty() {
            "not installed".into()
        } else {
            addon_lines
                .iter()
                .map(|w| {
                    format!(
                        "{} {}/{}",
                        w.name.trim_start_matches("gryvia-"),
                        w.ready,
                        w.desired
                    )
                })
                .collect::<Vec<_>>()
                .join(", ")
        },
        required: false,
    });

    // Collectors (optional)
    let (col_states, col_lines) = states_of(&is_collector);
    components.push(ComponentLine {
        name: "Collectors".into(),
        state: roll_up(&col_states),
        detail: if col_lines.is_empty() {
            "not installed".into()
        } else {
            col_lines
                .iter()
                .map(|w| format!("{}/{} ready", w.ready, w.desired))
                .collect::<Vec<_>>()
                .join(", ")
        },
        required: false,
    });

    let sched = if inputs.scheduler_backend.is_empty() {
        "default (kube-scheduler)".into()
    } else {
        inputs.scheduler_backend.clone()
    };
    components.push(ComponentLine {
        name: "Scheduler".into(),
        state: Marker::Ok,
        detail: sched,
        required: false,
    });

    let overall = components
        .iter()
        .filter(|c| c.required)
        .fold(Marker::Disabled, |acc, c| acc.worst(c.state));

    // Pods and their problems
    let pods_total = inputs.pods.len();
    let pods_running = inputs.pods.iter().filter(|p| pod_ok(p)).count();
    let mut errors: Vec<String> = inputs
        .pods
        .iter()
        .filter(|p| !pod_ok(p))
        .map(|p| {
            format!(
                "pod {}: {}",
                p.name,
                p.reason.clone().unwrap_or_else(|| p.phase.clone())
            )
        })
        .collect();
    for c in components
        .iter()
        .filter(|c| c.required && c.state == Marker::Error)
    {
        errors.push(format!("{}: {}", c.name, c.detail));
    }
    errors.sort();
    errors.dedup();

    // Image versions, one line per workload image
    let mut images: Vec<(String, String)> = inputs
        .workloads
        .iter()
        .flat_map(|w| w.images.iter().map(|i| (w.name.clone(), i.clone())))
        .collect();
    images.sort();

    StatusModel {
        namespace: inputs.namespace.clone(),
        overall,
        components,
        workloads,
        pods_running,
        pods_total,
        jobs: (!inputs.jobs_failed).then(|| inputs.jobs.clone()),
        images,
        nodes: build_node_rows(inputs),
        errors,
        warnings: inputs.warnings.clone(),
    }
}

/// One row per Kubernetes node, plus a row for any GryviaGpuNode whose node does not exist.
fn build_node_rows(inputs: &Inputs) -> Vec<NodeRow> {
    let mut names: Vec<String> = inputs.nodes.iter().map(|n| n.name.clone()).collect();
    for g in &inputs.gpu_nodes {
        if !names.contains(&g.node_name) {
            names.push(g.node_name.clone());
        }
    }
    names.sort();

    names
        .into_iter()
        .map(|name| {
            let node = inputs.nodes.iter().find(|n| n.name == name);
            let gpu = inputs.gpu_nodes.iter().find(|g| g.node_name == name);

            let (ready, ready_text) = match node {
                Some(n) if n.ready => (Marker::Ok, "Ready".to_string()),
                Some(_) => (Marker::Error, "NotReady".to_string()),
                None => (Marker::Warn, "no such node".to_string()),
            };
            let (gpus, driver, gpu_health, gpu_health_text) = match gpu {
                Some(g) => {
                    let gpus = if g.gpu_count > 0 {
                        format!("{} x {}", g.gpu_count, g.gpu_type)
                    } else {
                        "-".to_string()
                    };
                    let driver = match (g.driver.as_str(), g.cuda.as_str()) {
                        ("", "") => "-".to_string(),
                        (d, "") => d.to_string(),
                        ("", c) => format!("CUDA {c}"),
                        (d, c) => format!("{d} / CUDA {c}"),
                    };
                    let (m, text) = if g.gpus_reported == 0 {
                        (
                            if matches!(g.phase.as_str(), "Ready" | "Healthy" | "Active") {
                                Marker::Unknown
                            } else {
                                Marker::Warn
                            },
                            if g.phase.is_empty() {
                                "no data".to_string()
                            } else {
                                g.phase.clone()
                            },
                        )
                    } else if g.gpus_healthy == g.gpus_reported {
                        (
                            Marker::Ok,
                            format!("{}/{}", g.gpus_healthy, g.gpus_reported),
                        )
                    } else {
                        (
                            Marker::Warn,
                            format!("{}/{}", g.gpus_healthy, g.gpus_reported),
                        )
                    };
                    (gpus, driver, m, text)
                }
                None => ("-".into(), "-".into(), Marker::Disabled, "-".into()),
            };

            let node_pods: Vec<&PodInfo> = inputs
                .pods
                .iter()
                .filter(|p| p.node.as_deref() == Some(name.as_str()))
                .collect();
            let addons = NODE_ADDONS
                .iter()
                .map(|(key, needle)| {
                    let state = match node_pods.iter().find(|p| p.name.contains(needle)) {
                        Some(p) if pod_ok(p) => Marker::Ok,
                        Some(_) => Marker::Error,
                        None => Marker::Disabled,
                    };
                    (key.to_string(), state)
                })
                .collect();

            NodeRow {
                maintenance: node.and_then(|n| {
                    n.maintenance
                        .clone()
                        .or_else(|| n.unschedulable.then(|| "cordoned".to_string()))
                }),
                name,
                ready,
                ready_text,
                gpus,
                driver,
                gpu_health,
                gpu_health_text,
                addons,
                pods: node_pods.len(),
            }
        })
        .collect()
}

// ── Rendering ────────────────────────────────────────────────────────────────────────────────────

/// A small hexagon logo, five rows, drawn beside the component list.
const LOGO: [&str; 5] = [
    "    ______    ",
    "   /      \\   ",
    "  /   G    \\  ",
    "  \\        /  ",
    "   \\______/   ",
];

fn logo_row(i: usize, color: bool) -> String {
    let row = LOGO.get(i).copied().unwrap_or("              ");
    ui::ansi(row, "1;34", color)
}

/// The full status text. `color` selects ANSI styling.
pub fn render(model: &StatusModel, color: bool) -> String {
    let mut out = String::new();

    // Banner: logo on the left, "Gryvia: OK" and one line per component on the right.
    let mut right: Vec<String> = Vec::new();
    let name_w = 14;
    right.push(format!(
        "{}{}",
        ui::ansi(&format!("{:<name_w$}", "Gryvia:"), "1", color),
        model.overall.paint_with(model.overall.label(), color)
    ));
    for c in &model.components {
        let detail = if c.detail.is_empty() {
            String::new()
        } else {
            format!("  {}", ui::ansi(&c.detail, "2", color))
        };
        right.push(format!(
            "{}{}{}",
            ui::ansi(&format!("{:<name_w$}", format!("{}:", c.name)), "1", color),
            c.state.paint_with(c.state.label(), color),
            detail
        ));
    }
    let rows = right.len().max(LOGO.len());
    for i in 0..rows {
        out.push_str(&logo_row(i, color));
        out.push_str("  ");
        if let Some(line) = right.get(i) {
            out.push_str(line);
        }
        out.push('\n');
    }
    out.push('\n');

    // Workloads
    let kind_w = 11;
    let name_w = model
        .workloads
        .iter()
        .map(|w| w.name.len())
        .max()
        .unwrap_or(0)
        + 2;
    for w in &model.workloads {
        let numbers = format!(
            "Desired: {}, Ready: {}/{}, Available: {}/{}",
            w.desired, w.ready, w.desired, w.available, w.desired
        );
        out.push_str(&format!(
            "{}{}{}\n",
            ui::ansi(&format!("{:<kind_w$}", w.kind.label()), "2", color),
            ui::ansi(&format!("{:<name_w$}", w.name), "1", color),
            w.state.paint_with(&numbers, color)
        ));
    }
    out.push('\n');

    // Cluster totals
    let pods_state = if model.pods_total == 0 {
        Marker::Disabled
    } else if model.pods_running == model.pods_total {
        Marker::Ok
    } else {
        Marker::Warn
    };
    let line = |key: &str, value: &str| -> String {
        format!("{}{}\n", ui::ansi(&format!("{key:<16}"), "1", color), value)
    };
    out.push_str(&line(
        "Cluster Pods:",
        &pods_state.paint_with(
            &format!("{}/{} running", model.pods_running, model.pods_total),
            color,
        ),
    ));
    let jobs_text = match &model.jobs {
        Some(j) => format!(
            "{} running, {} pending, {} completed, {} failed",
            j.running, j.pending, j.completed, j.failed
        ),
        None => Marker::Unknown.paint_with("unavailable", color),
    };
    out.push_str(&line("Jobs:", &jobs_text));
    out.push_str(&line("Namespace:", &model.namespace));

    // Image versions
    if !model.images.is_empty() {
        out.push_str(&format!("\n{}\n", ui::section("Image versions", color)));
        let w = model.images.iter().map(|(n, _)| n.len()).max().unwrap_or(0) + 2;
        for (name, image) in &model.images {
            out.push_str(&format!("  {name:<w$}{image}\n"));
        }
    }

    // Warnings and errors
    if !model.warnings.is_empty() {
        out.push_str(&format!("\n{}\n", ui::section("Warnings", color)));
        for w in &model.warnings {
            out.push_str(&format!(
                "  {} {}\n",
                Marker::Warn.paint_with(Marker::Warn.glyph(), color),
                w
            ));
        }
    }
    if !model.errors.is_empty() {
        out.push_str(&format!("\n{}\n", ui::section("Errors", color)));
        for e in &model.errors {
            out.push_str(&format!(
                "  {} {}\n",
                Marker::Error.paint_with(Marker::Error.glyph(), color),
                e
            ));
        }
    }

    // Nodes
    if !model.nodes.is_empty() {
        out.push_str(&format!("\n{}\n", ui::section("Nodes", color)));
        let any_notes = model.nodes.iter().any(|n| n.maintenance.is_some());
        let mut headers = vec![
            "NODE",
            "STATE",
            "GPUS",
            "DRIVER",
            "GPU HEALTH",
            "DCGM",
            "PLUGIN",
            "COLLECTOR",
            "PODS",
        ];
        if any_notes {
            headers.push("NOTES");
        }
        let rows: Vec<Vec<Cell2>> = model
            .nodes
            .iter()
            .map(|n| {
                let addon = |key: &str| -> Cell2 {
                    let m = n.addons.get(key).copied().unwrap_or(Marker::Disabled);
                    (m.glyph().to_string(), Some(m))
                };
                let mut row = vec![
                    (n.name.clone(), None),
                    (n.ready_text.clone(), Some(n.ready)),
                    (n.gpus.clone(), None),
                    (n.driver.clone(), None),
                    (n.gpu_health_text.clone(), Some(n.gpu_health)),
                    addon("dcgm"),
                    addon("plugin"),
                    addon("collector"),
                    (n.pods.to_string(), None),
                ];
                if any_notes {
                    row.push((
                        n.maintenance.clone().unwrap_or_default(),
                        Some(Marker::Warn),
                    ));
                }
                row
            })
            .collect();
        for line in ui::grid(&headers, &rows, color) {
            out.push_str(&format!("  {line}\n"));
        }
    }
    out
}

/// `--brief`: `OK`, or one line per failing component.
pub fn render_brief(model: &StatusModel, color: bool) -> String {
    if !model.failed() {
        return format!("{}\n", Marker::Ok.paint_with("OK", color));
    }
    let mut out = String::new();
    for c in model
        .components
        .iter()
        .filter(|c| c.required && c.state == Marker::Error)
    {
        out.push_str(&format!(
            "{} {}: {}\n",
            Marker::Error.paint_with(Marker::Error.glyph(), color),
            c.name,
            c.detail
        ));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn deploy(name: &str, desired: i32, ready: i32) -> WorkloadInfo {
        WorkloadInfo {
            kind: WorkloadKind::Deployment,
            name: name.into(),
            desired,
            ready,
            available: ready,
            images: vec![format!("ghcr.io/zyvorai/{name}:1.0.0")],
        }
    }

    fn daemonset(name: &str, desired: i32, ready: i32) -> WorkloadInfo {
        WorkloadInfo {
            kind: WorkloadKind::DaemonSet,
            ..deploy(name, desired, ready)
        }
    }

    fn pod(name: &str, node: Option<&str>, ready: bool, reason: Option<&str>) -> PodInfo {
        PodInfo {
            name: name.into(),
            node: node.map(String::from),
            phase: if ready {
                "Running".into()
            } else {
                "Pending".into()
            },
            ready,
            reason: reason.map(String::from),
        }
    }

    fn node(name: &str) -> NodeInfo {
        NodeInfo {
            name: name.into(),
            ready: true,
            unschedulable: false,
            maintenance: None,
        }
    }

    fn gpu_node(name: &str, phase: &str, healthy: u32, reported: u32) -> GpuNodeInfo {
        GpuNodeInfo {
            node_name: name.into(),
            gpu_type: "H100".into(),
            gpu_count: 8,
            phase: phase.into(),
            driver: "550.54".into(),
            cuda: "12.4".into(),
            gpus_reported: reported,
            gpus_healthy: healthy,
        }
    }

    fn healthy() -> Inputs {
        Inputs {
            namespace: "gryvia-system".into(),
            workloads: vec![
                deploy("gryvia-gpu-operator", 1, 1),
                deploy("gryvia-ai-operator", 1, 1),
                deploy("gryvia-quota-operator", 1, 1),
                deploy("gryvia-api-gateway", 2, 2),
                deploy("gryvia-ui", 2, 2),
                daemonset("gryvia-dcgm-exporter", 2, 2),
                daemonset("gryvia-nvidia-device-plugin", 2, 2),
            ],
            pods: vec![
                pod("gryvia-gpu-operator-abc", Some("node-a"), true, None),
                pod("gryvia-dcgm-exporter-x1", Some("node-a"), true, None),
                pod("gryvia-nvidia-device-plugin-x1", Some("node-a"), true, None),
                pod("gryvia-dcgm-exporter-x2", Some("node-b"), true, None),
                pod("gryvia-nvidia-device-plugin-x2", Some("node-b"), true, None),
            ],
            nodes: vec![node("node-a"), node("node-b")],
            gpu_nodes: vec![
                gpu_node("node-a", "Ready", 8, 8),
                gpu_node("node-b", "Ready", 8, 8),
            ],
            jobs: JobCounts {
                running: 1,
                pending: 2,
                completed: 3,
                failed: 0,
            },
            warnings: vec![],
            ..Inputs::default()
        }
    }

    fn component<'a>(m: &'a StatusModel, name: &str) -> &'a ComponentLine {
        m.components.iter().find(|c| c.name == name).unwrap()
    }

    #[test]
    fn a_healthy_platform_is_ok_and_does_not_fail() {
        let m = build_model(&healthy());
        assert_eq!(m.overall, Marker::Ok);
        assert!(!m.failed());
        assert_eq!(component(&m, "Operators").state, Marker::Ok);
        assert_eq!(component(&m, "Operators").detail, "gpu, ai, quota");
        assert_eq!(component(&m, "GPU nodes").detail, "2/2 ready");
        assert_eq!(component(&m, "GPU add-ons").state, Marker::Ok);
        assert_eq!(component(&m, "Collectors").state, Marker::Disabled);
        assert!(m.errors.is_empty());
        assert_eq!((m.pods_running, m.pods_total), (5, 5));
    }

    #[test]
    fn a_missing_operator_fails_the_platform() {
        let mut i = healthy();
        i.workloads.retain(|w| !w.name.ends_with("-operator"));
        let m = build_model(&i);
        assert!(m.failed());
        assert_eq!(component(&m, "Operators").state, Marker::Error);
        assert!(m.errors.iter().any(|e| e == "Operators: not installed"));
        assert!(render_brief(&m, false).contains("Operators: not installed"));
    }

    #[test]
    fn a_partly_ready_gateway_is_a_warning_not_a_failure() {
        let mut i = healthy();
        i.workloads
            .iter_mut()
            .find(|w| w.name == "gryvia-api-gateway")
            .unwrap()
            .ready = 1;
        let m = build_model(&i);
        assert_eq!(component(&m, "API gateway").state, Marker::Warn);
        assert_eq!(m.overall, Marker::Warn);
        assert!(!m.failed());
    }

    #[test]
    fn a_gateway_with_nothing_ready_fails_and_names_the_pod_problem() {
        let mut i = healthy();
        i.workloads
            .iter_mut()
            .find(|w| w.name == "gryvia-api-gateway")
            .unwrap()
            .ready = 0;
        i.pods.push(pod(
            "gryvia-api-gateway-zzz",
            Some("node-a"),
            false,
            Some("ImagePullBackOff"),
        ));
        let m = build_model(&i);
        assert!(m.failed());
        assert!(m
            .errors
            .contains(&"pod gryvia-api-gateway-zzz: ImagePullBackOff".to_string()));
        assert_eq!((m.pods_running, m.pods_total), (5, 6));
    }

    #[test]
    fn optional_components_never_fail_the_exit_code() {
        let mut i = healthy();
        i.gpu_nodes = vec![gpu_node("node-a", "Failed", 0, 0)];
        i.workloads.push(daemonset("gryvia-collector", 2, 0));
        let m = build_model(&i);
        assert_eq!(component(&m, "GPU nodes").state, Marker::Warn);
        assert_eq!(component(&m, "Collectors").state, Marker::Error);
        assert!(!m.failed());
    }

    #[test]
    fn nodes_are_joined_with_their_gpu_node_and_addon_pods() {
        let mut i = healthy();
        i.nodes[1].ready = false;
        i.nodes[0].unschedulable = true;
        i.gpu_nodes[1] = gpu_node("node-b", "Ready", 6, 8);
        i.pods[3] = pod(
            "gryvia-dcgm-exporter-x2",
            Some("node-b"),
            false,
            Some("CrashLoopBackOff"),
        );
        let m = build_model(&i);
        let a = m.nodes.iter().find(|n| n.name == "node-a").unwrap();
        let b = m.nodes.iter().find(|n| n.name == "node-b").unwrap();
        assert_eq!(a.gpus, "8 x H100");
        assert_eq!(a.driver, "550.54 / CUDA 12.4");
        assert_eq!(a.maintenance.as_deref(), Some("cordoned"));
        assert_eq!(a.addons["dcgm"], Marker::Ok);
        assert_eq!(a.addons["collector"], Marker::Disabled);
        assert_eq!(b.ready, Marker::Error);
        assert_eq!(b.gpu_health, Marker::Warn);
        assert_eq!(b.gpu_health_text, "6/8");
        assert_eq!(b.addons["dcgm"], Marker::Error);
        assert_eq!(b.pods, 2);
    }

    #[test]
    fn a_gpu_node_without_a_kubernetes_node_gets_its_own_row() {
        let mut i = healthy();
        i.gpu_nodes.push(gpu_node("ghost", "Failed", 0, 0));
        let m = build_model(&i);
        let ghost = m.nodes.iter().find(|n| n.name == "ghost").unwrap();
        assert_eq!(ghost.ready_text, "no such node");
        assert_eq!(ghost.ready, Marker::Warn);
        assert_eq!(ghost.gpu_health_text, "Failed");
    }

    #[test]
    fn unreadable_lists_are_unknown_not_empty() {
        let mut i = healthy();
        i.gpu_nodes.clear();
        i.gpu_nodes_failed = true;
        i.jobs_failed = true;
        i.warnings
            .push("could not list jobs: the gryvia.io/v1alpha1 API is not available".into());
        let m = build_model(&i);
        assert_eq!(component(&m, "GPU nodes").state, Marker::Unknown);
        assert_eq!(component(&m, "GPU nodes").detail, "unavailable");
        assert!(m.jobs.is_none());
        assert!(
            !m.failed(),
            "an unreadable optional list must not fail the platform"
        );
        let text = render(&m, false);
        assert!(text.contains("Jobs:           unavailable"));
        assert!(text.contains("GPU nodes:    unknown  unavailable"));
        let json = serde_json::to_value(&m).unwrap();
        assert!(json["jobs"].is_null());
    }

    #[test]
    fn only_node_filters_the_rows() {
        let mut m = build_model(&healthy());
        m.only_node("node-b");
        assert_eq!(m.nodes.len(), 1);
        assert_eq!(m.nodes[0].name, "node-b");
    }

    /// Compare `text` with `tests/snapshots/<name>`. Regenerate with `UPDATE_SNAPSHOTS=1 cargo test`.
    fn assert_snapshot(name: &str, text: &str) {
        let path = format!("{}/tests/snapshots/{name}", env!("CARGO_MANIFEST_DIR"));
        if std::env::var("UPDATE_SNAPSHOTS").is_ok() {
            std::fs::write(&path, text).unwrap();
        }
        let expected = std::fs::read_to_string(&path)
            .unwrap_or_else(|_| panic!("missing snapshot {path}; run with UPDATE_SNAPSHOTS=1"));
        assert_eq!(
            text, expected,
            "snapshot {name} differs (UPDATE_SNAPSHOTS=1 to accept)"
        );
    }

    #[test]
    fn healthy_snapshot() {
        let text = render(&build_model(&healthy()), false);
        assert!(!text.contains('\x1b'));
        assert_snapshot("status_healthy.txt", &text);
    }

    #[test]
    fn degraded_snapshot() {
        let mut i = healthy();
        i.workloads.retain(|w| w.name != "gryvia-quota-operator");
        let gateway = i
            .workloads
            .iter_mut()
            .find(|w| w.name == "gryvia-api-gateway")
            .unwrap();
        gateway.ready = 1;
        gateway.available = 1;
        i.pods.push(pod(
            "gryvia-quota-operator-q1",
            Some("node-b"),
            false,
            Some("ImagePullBackOff"),
        ));
        i.nodes[0].maintenance = Some("driver update".into());
        i.nodes[0].unschedulable = true;
        i.nodes[1].ready = false;
        i.gpu_nodes[1] = gpu_node("node-b", "Ready", 6, 8);
        i.gpu_nodes.push(gpu_node("ghost", "Failed", 0, 0));
        i.warnings.push("could not list jobs: forbidden".into());
        let text = render(&build_model(&i), false);
        assert_snapshot("status_degraded.txt", &text);
    }

    #[test]
    fn colored_render_has_escape_codes_and_the_same_text() {
        let m = build_model(&healthy());
        let plain = render(&m, false);
        let colored = render(&m, true);
        assert!(colored.contains("\x1b["));
        let mut stripped = String::new();
        let mut skip = false;
        for c in colored.chars() {
            if c == '\x1b' {
                skip = true;
            } else if skip && c == 'm' {
                skip = false;
            } else if !skip {
                stripped.push(c);
            }
        }
        assert_eq!(stripped, plain);
    }

    #[test]
    fn errors_section_lists_the_problems() {
        let mut i = healthy();
        i.workloads.retain(|w| w.name != "gryvia-ui");
        let text = render(&build_model(&i), false);
        assert!(text.contains("Errors"));
        assert!(text.contains("Dashboard: not installed"));
    }

    #[test]
    fn brief_is_ok_when_nothing_failed() {
        assert_eq!(render_brief(&build_model(&healthy()), false), "OK\n");
    }
}
