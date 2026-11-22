// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia models watch list|runs|create|suspend|resume|delete`: GryviaModelWatch objects (namespaced)
//! through the Kubernetes API. The ai-operator polls the model hub for each watch and starts one
//! GryviaWorkflow per new model; `runs` shows those candidates. Needs the ai-operator running with
//! `--enable-model-watch` (see docs/model-factory.md).
//!
//! `gryvia models rollback <entry>` asks the operator to put a shared service back on the entry's
//! status.previousVersion (annotation gryvia.io/rollback-requested).

use anyhow::{anyhow, bail, Context, Result};
use dialoguer::Confirm;
use kube::api::{
    Api, ApiResource, DeleteParams, DynamicObject, GroupVersionKind, ListParams, Patch,
    PatchParams, PostParams,
};
use serde_json::{json, Value};

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

const KIND: &str = "GryviaModelWatch";

fn api(client: &GryviaClient) -> Api<DynamicObject> {
    let gvk = GroupVersionKind::gvk("gryvia.io", "v1alpha1", KIND);
    let ar = ApiResource::from_gvk_with_plural(&gvk, "gryviamodelwatches");
    Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar)
}

fn api_error(what: &str, e: kube::Error) -> anyhow::Error {
    anyhow!("{what}: {}", display::cluster_error_text(&e.to_string()))
}

fn str_at<'a>(v: &'a Value, ptr: &str) -> &'a str {
    v.pointer(ptr).and_then(Value::as_str).unwrap_or("")
}

fn or_dash(s: &str) -> String {
    if s.is_empty() {
        "-".to_string()
    } else {
        s.to_string()
    }
}

/// The marker for a watch phase or a candidate phase.
pub fn phase_marker(phase: &str) -> Marker {
    match phase {
        "Watching" | "Running" | "Succeeded" => Marker::Ok,
        "Queued" => Marker::Warn,
        "Failed" => Marker::Error,
        "Suspended" | "Rejected" | "Baseline" => Marker::Disabled,
        _ => Marker::Unknown,
    }
}

pub fn watch_list_lines(items: &[Value], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("Model Watches", color), String::new()];
    if items.is_empty() {
        lines.push("No model watches found".to_string());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = items
        .iter()
        .map(|w| {
            let sources: Vec<String> = w
                .pointer("/spec/sources")
                .and_then(Value::as_array)
                .map(|a| {
                    a.iter()
                        .map(|s| match str_at(s, "/author") {
                            "" => str_at(s, "/provider").to_string(),
                            author => author.to_string(),
                        })
                        .collect()
                })
                .unwrap_or_default();
            let cands = w
                .pointer("/status/candidates")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default();
            let count = |phase: &str| {
                cands
                    .iter()
                    .filter(|c| str_at(c, "/phase") == phase)
                    .count()
            };
            let phase = or_dash(str_at(w, "/status/phase"));
            vec![
                (str_at(w, "/metadata/name").to_string(), None),
                (or_dash(&sources.join(",")), None),
                (phase.clone(), Some(phase_marker(&phase))),
                (
                    w.pointer("/status/activeRuns")
                        .and_then(Value::as_u64)
                        .unwrap_or(0)
                        .to_string(),
                    None,
                ),
                (count("Succeeded").to_string(), None),
                (count("Failed").to_string(), None),
                (or_dash(str_at(w, "/status/lastPollTime")), None),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &[
            "NAME",
            "SOURCES",
            "PHASE",
            "ACTIVE",
            "SUCCEEDED",
            "FAILED",
            "LAST POLL",
        ],
        &rows,
        color,
    ));
    lines.push(String::new());
    lines.push(format!("Total: {} model watches", items.len()));
    lines
}

/// Candidates of one watch, newest first. Baseline entries (models that existed before the watch)
/// are hidden unless `all` is set.
pub fn run_lines(watch: &Value, all: bool, color: bool) -> Vec<String> {
    let name = str_at(watch, "/metadata/name");
    let mut lines = vec![ui::header(&format!("Runs of {name}"), color), String::new()];
    let cands: Vec<&Value> = watch
        .pointer("/status/candidates")
        .and_then(Value::as_array)
        .map(|a| {
            a.iter()
                .rev()
                .filter(|c| all || str_at(c, "/phase") != "Baseline")
                .collect()
        })
        .unwrap_or_default();
    if cands.is_empty() {
        lines.push("No runs yet".to_string());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = cands
        .iter()
        .map(|c| {
            let phase = str_at(c, "/phase").to_string();
            let rev: String = str_at(c, "/revision").chars().take(7).collect();
            let gpus = c
                .pointer("/gpus")
                .and_then(Value::as_u64)
                .map(|g| g.to_string());
            let detail = match (str_at(c, "/workflow"), str_at(c, "/message")) {
                ("", m) => or_dash(m),
                (w, "") => w.to_string(),
                (w, m) => format!("{w} ({m})"),
            };
            vec![
                (str_at(c, "/id").to_string(), None),
                (or_dash(&rev), None),
                (or_dash(str_at(c, "/paramsB")), None),
                (gpus.unwrap_or_else(|| "-".to_string()), None),
                (phase.clone(), Some(phase_marker(&phase))),
                (detail, None),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &[
            "MODEL",
            "REVISION",
            "PARAMS(B)",
            "GPUs",
            "PHASE",
            "WORKFLOW / REASON",
        ],
        &rows,
        color,
    ));
    lines
}

/// The GryviaModelWatch documents of a (possibly multi-document) YAML file; other kinds are skipped
/// and returned by name so the caller can say so.
pub fn watches_from_yaml(text: &str) -> Result<(Vec<Value>, Vec<String>)> {
    let mut watches = Vec::new();
    let mut skipped = Vec::new();
    for doc in serde_yaml::Deserializer::from_str(text) {
        let v: Value = serde::Deserialize::deserialize(doc).context("Failed to parse YAML")?;
        if v.is_null() {
            continue;
        }
        let kind = str_at(&v, "/kind");
        if kind == KIND {
            if str_at(&v, "/metadata/name").is_empty() {
                bail!("a {KIND} document has no metadata.name");
            }
            watches.push(v);
        } else {
            skipped.push(format!(
                "{} {}",
                or_dash(kind),
                str_at(&v, "/metadata/name")
            ));
        }
    }
    if watches.is_empty() {
        bail!("no {KIND} document found");
    }
    Ok((watches, skipped))
}

pub async fn list(client: &GryviaClient, output: &str) -> Result<()> {
    let items = api(client)
        .list(&ListParams::default())
        .await
        .map_err(|e| api_error("Failed to list model watches", e))?
        .items;
    if output != "table" {
        return crate::output::print_serialized(output, &items);
    }
    let values: Vec<Value> = items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .collect();
    for line in watch_list_lines(&values, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

pub async fn runs(client: &GryviaClient, name: &str, all: bool, output: &str) -> Result<()> {
    let obj = api(client)
        .get(name)
        .await
        .map_err(|e| api_error(&format!("Failed to get model watch '{name}'"), e))?;
    let watch = serde_json::to_value(&obj)?;
    if output != "table" {
        let cands = watch
            .pointer("/status/candidates")
            .cloned()
            .unwrap_or(json!([]));
        return crate::output::print_serialized(output, &cands);
    }
    for line in run_lines(&watch, all, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

pub async fn create(client: &GryviaClient, file: &str) -> Result<()> {
    let text = std::fs::read_to_string(file).with_context(|| format!("Failed to read {file}"))?;
    let (watches, skipped) = watches_from_yaml(&text)?;
    for s in skipped {
        display::print_info(&format!("Skipped {s} (apply it with kubectl)"));
    }
    let api = api(client);
    for mut w in watches {
        if let Some(meta) = w.get_mut("metadata").and_then(Value::as_object_mut) {
            meta.remove("namespace");
        }
        let name = str_at(&w, "/metadata/name").to_string();
        let obj: DynamicObject = serde_json::from_value(w)?;
        api.create(&PostParams::default(), &obj)
            .await
            .map_err(|e| api_error(&format!("Failed to create model watch '{name}'"), e))?;
        display::print_success(&format!(
            "Model watch {name} created; see its runs with: gryvia models watch runs {name}"
        ));
    }
    Ok(())
}

pub async fn set_suspend(client: &GryviaClient, name: &str, suspend: bool) -> Result<()> {
    api(client)
        .patch(
            name,
            &PatchParams::default(),
            &Patch::Merge(json!({"spec": {"suspend": suspend}})),
        )
        .await
        .map_err(|e| api_error(&format!("Failed to update model watch '{name}'"), e))?;
    if suspend {
        display::print_success(&format!(
            "Model watch {name} suspended; running workflows continue"
        ));
    } else {
        display::print_success(&format!("Model watch {name} resumed"));
    }
    Ok(())
}

pub async fn delete(client: &GryviaClient, name: &str, yes: bool) -> Result<()> {
    if !yes {
        let confirm = Confirm::new()
            .with_prompt(format!(
                "Delete model watch '{name}'? Its workflows are deleted too; registered models are kept"
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
        .map_err(|e| api_error(&format!("Failed to delete model watch '{name}'"), e))?;
    display::print_success(&format!("Model watch {name} deleted"));
    Ok(())
}

fn registry_api(client: &GryviaClient) -> Api<DynamicObject> {
    let gvk = GroupVersionKind::gvk("gryvia.io", "v1alpha1", "GryviaModelRegistry");
    let ar = ApiResource::from_gvk_with_plural(&gvk, "gryviamodelregistries");
    Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar)
}

/// The version a rollback of this registry entry brings back, or why it cannot be rolled back.
pub fn rollback_target(entry: &Value) -> std::result::Result<String, String> {
    let name = str_at(entry, "/metadata/name");
    if str_at(entry, "/spec/stage") != "production"
        || str_at(entry, "/spec/servingConfig/serviceName").is_empty()
    {
        return Err(format!(
            "Model '{name}' is not a production entry with servingConfig.serviceName; only those can be rolled back"
        ));
    }
    match str_at(entry, "/status/previousVersion") {
        "" => Err(format!(
            "Model '{name}' has no previous version to roll back to"
        )),
        prev => Ok(prev.to_string()),
    }
}

pub async fn rollback(client: &GryviaClient, name: &str, yes: bool) -> Result<()> {
    let api = registry_api(client);
    let obj = api
        .get(name)
        .await
        .map_err(|e| api_error(&format!("Failed to get model '{name}'"), e))?;
    let previous = rollback_target(&serde_json::to_value(&obj)?).map_err(|e| anyhow!(e))?;
    if !yes {
        let confirm = Confirm::new()
            .with_prompt(format!(
                "Roll back '{name}'? Its shared service serves '{previous}' again and '{name}' is archived"
            ))
            .interact()?;
        if !confirm {
            display::print_info("Cancelled");
            return Ok(());
        }
    }
    let when = chrono::Utc::now().format("%Y-%m-%dT%H:%M:%SZ");
    api.patch(
        name,
        &PatchParams::default(),
        &Patch::Merge(json!({"metadata": {"annotations": {"gryvia.io/rollback-requested": format!("cli at {when}")}}})),
    )
    .await
    .map_err(|e| api_error(&format!("Failed to update model '{name}'"), e))?;
    display::print_success(&format!(
        "Rollback of {name} to {previous} requested; the RolledBack condition shows the outcome: kubectl get gryviamodelregistry {name} -o yaml"
    ));
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rollback_needs_a_serving_entry_with_a_previous_version() {
        let entry = |stage: &str, service: &str, prev: &str| {
            json!({"metadata": {"name": "chat-v2"},
                   "spec": {"stage": stage, "servingConfig": {"serviceName": service}},
                   "status": {"previousVersion": prev}})
        };
        assert_eq!(
            rollback_target(&entry("production", "chat", "chat-v1")),
            Ok("chat-v1".to_string())
        );
        assert!(rollback_target(&entry("staging", "chat", "chat-v1"))
            .unwrap_err()
            .contains("not a production entry"));
        assert!(rollback_target(&entry("production", "", "chat-v1")).is_err());
        assert!(rollback_target(&entry("production", "chat", ""))
            .unwrap_err()
            .contains("no previous version"));
    }

    fn watch() -> Value {
        json!({
            "metadata": {"name": "open-llms"},
            "spec": {"sources": [{"provider": "huggingface", "author": "Qwen"}, {"provider": "huggingface"}]},
            "status": {"phase": "Watching", "activeRuns": 1, "lastPollTime": "2026-01-01T00:00:00Z",
                       "candidates": [
                           {"id": "Qwen/Qwen3-0.5B", "revision": "aaaaaaaaaa", "phase": "Baseline"},
                           {"id": "Qwen/Qwen3-8B", "revision": "bbbbbbbbbb", "paramsB": "8", "gpus": 1,
                            "phase": "Running", "workflow": "open-llms-qwen3-8b-bbbbbbb"},
                           {"id": "Qwen/Qwen3-72B", "revision": "cccccccccc", "paramsB": "72",
                            "phase": "Rejected", "message": "72B parameters exceeds maxParamsB 14"}]}
        })
    }

    #[test]
    fn list_report() {
        let text = watch_list_lines(&[watch()], false).join("\n");
        let expected = "\
━━━ Model Watches ━━━

NAME       SOURCES           PHASE     ACTIVE  SUCCEEDED  FAILED  LAST POLL
open-llms  Qwen,huggingface  Watching  1       0          0       2026-01-01T00:00:00Z

Total: 1 model watches";
        assert_eq!(text, expected);
        assert!(watch_list_lines(&[], false)
            .join("\n")
            .contains("No model watches found"));
    }

    #[test]
    fn runs_newest_first_without_baseline() {
        let text = run_lines(&watch(), false, false).join("\n");
        let expected = "\
━━━ Runs of open-llms ━━━

MODEL           REVISION  PARAMS(B)  GPUs  PHASE     WORKFLOW / REASON
Qwen/Qwen3-72B  ccccccc   72         -     Rejected  72B parameters exceeds maxParamsB 14
Qwen/Qwen3-8B   bbbbbbb   8          1     Running   open-llms-qwen3-8b-bbbbbbb";
        assert_eq!(text, expected);
        assert!(run_lines(&watch(), true, false)
            .join("\n")
            .contains("Baseline"));
    }

    #[test]
    fn yaml_picks_watches() {
        let text = "apiVersion: v1\nkind: Secret\nmetadata: {name: hf-token}\n---\n\
                    apiVersion: gryvia.io/v1alpha1\nkind: GryviaModelWatch\nmetadata: {name: w, namespace: x}\nspec: {}\n";
        let (w, skipped) = watches_from_yaml(text).unwrap();
        assert_eq!(w.len(), 1);
        assert_eq!(skipped, vec!["Secret hf-token".to_string()]);
        assert!(watches_from_yaml("kind: Secret\nmetadata: {name: s}\n").is_err());
        assert!(watches_from_yaml("kind: GryviaModelWatch\nmetadata: {}\n").is_err());
    }

    #[test]
    fn markers() {
        assert_eq!(phase_marker("Watching"), Marker::Ok);
        assert_eq!(phase_marker("Queued"), Marker::Warn);
        assert_eq!(phase_marker("Failed"), Marker::Error);
        assert_eq!(phase_marker("Baseline"), Marker::Disabled);
        assert_eq!(phase_marker("other"), Marker::Unknown);
    }
}
