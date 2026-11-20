// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! Shared list/get/create/delete of one Gryvia kind through the Kubernetes API, for the commands that only
//! need those verbs (`gryvia datasets`, `gryvia rag index`, `gryvia agents`). Each command describes its kind
//! with a [`KindSpec`]: the plural, whether it is cluster-scoped, and the table columns.

use anyhow::{anyhow, bail, Context, Result};
use dialoguer::Confirm;
use kube::api::{
    Api, ApiResource, DeleteParams, DynamicObject, GroupVersionKind, ListParams, PostParams,
};
use serde_json::{json, Value};

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

/// One table column: a JSON pointer into the object; `marker` colours the cell by [`KindSpec::marker`].
pub struct Column {
    pub header: &'static str,
    pub ptr: &'static str,
    pub marker: bool,
}

pub struct KindSpec {
    pub kind: &'static str,
    pub plural: &'static str,
    pub cluster: bool,
    /// Singular noun for messages ("dataset").
    pub noun: &'static str,
    /// Table title ("Datasets").
    pub title: &'static str,
    pub columns: &'static [Column],
    pub marker: fn(&str) -> Marker,
}

pub fn api(client: &GryviaClient, k: &KindSpec) -> Api<DynamicObject> {
    let gvk = GroupVersionKind::gvk("gryvia.io", "v1alpha1", k.kind);
    let ar = ApiResource::from_gvk_with_plural(&gvk, k.plural);
    if k.cluster {
        Api::all_with(client.kube_client.clone(), &ar)
    } else {
        Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar)
    }
}

pub fn api_error(what: &str, e: kube::Error) -> anyhow::Error {
    anyhow!("{what}: {}", display::cluster_error_text(&e.to_string()))
}

/// The text of a value at `ptr`: strings as-is, numbers and booleans formatted, "-" when absent.
pub fn text_at(v: &Value, ptr: &str) -> String {
    match v.pointer(ptr) {
        Some(Value::String(s)) if !s.is_empty() => s.clone(),
        Some(Value::Number(n)) => n.to_string(),
        Some(Value::Bool(b)) => b.to_string(),
        Some(Value::Array(a)) if !a.is_empty() && a.iter().all(Value::is_string) => a
            .iter()
            .filter_map(Value::as_str)
            .collect::<Vec<_>>()
            .join(","),
        _ => "-".to_string(),
    }
}

pub fn list_lines(k: &KindSpec, items: &[Value], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header(k.title, color), String::new()];
    if items.is_empty() {
        lines.push(format!("No {}s found", k.noun));
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = items
        .iter()
        .map(|o| {
            k.columns
                .iter()
                .map(|c| {
                    let t = text_at(o, c.ptr);
                    let m = c.marker.then(|| (k.marker)(&t));
                    (t, m)
                })
                .collect()
        })
        .collect();
    let headers: Vec<&str> = k.columns.iter().map(|c| c.header).collect();
    lines.extend(ui::grid(&headers, &rows, color));
    lines.push(String::new());
    lines.push(format!("Total: {} {}s", items.len(), k.noun));
    lines
}

/// The documents of `k.kind` in a (possibly multi-document) YAML file; other kinds are skipped and returned
/// as "Kind name" so the caller can say so.
pub fn objects_from_yaml(k: &KindSpec, text: &str) -> Result<(Vec<Value>, Vec<String>)> {
    let mut objs = Vec::new();
    let mut skipped = Vec::new();
    for doc in serde_yaml::Deserializer::from_str(text) {
        let v: Value = serde::Deserialize::deserialize(doc).context("Failed to parse YAML")?;
        if v.is_null() {
            continue;
        }
        let kind = v.pointer("/kind").and_then(Value::as_str).unwrap_or("");
        let name = v
            .pointer("/metadata/name")
            .and_then(Value::as_str)
            .unwrap_or("");
        if kind == k.kind {
            if name.is_empty() {
                bail!("a {} document has no metadata.name", k.kind);
            }
            objs.push(v);
        } else {
            let kind = if kind.is_empty() { "-" } else { kind };
            skipped.push(format!("{kind} {name}"));
        }
    }
    if objs.is_empty() {
        bail!("no {} document found", k.kind);
    }
    Ok((objs, skipped))
}

pub async fn list(client: &GryviaClient, k: &KindSpec, output: &str) -> Result<()> {
    let items = api(client, k)
        .list(&ListParams::default())
        .await
        .map_err(|e| api_error(&format!("Failed to list {}s", k.noun), e))?
        .items;
    if output != "table" {
        return crate::output::print_serialized(output, &items);
    }
    let values: Vec<Value> = items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .collect();
    for line in list_lines(k, &values, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

pub async fn get_value(client: &GryviaClient, k: &KindSpec, name: &str) -> Result<Value> {
    let obj = api(client, k)
        .get(name)
        .await
        .map_err(|e| api_error(&format!("Failed to get {} '{name}'", k.noun), e))?;
    Ok(serde_json::to_value(&obj)?)
}

/// Prints the spec and status (YAML for the table format, which has no single-object table).
pub async fn get(client: &GryviaClient, k: &KindSpec, name: &str, output: &str) -> Result<()> {
    let v = get_value(client, k, name).await?;
    let view = json!({
        "name": v.pointer("/metadata/name"),
        "namespace": v.pointer("/metadata/namespace"),
        "spec": v.pointer("/spec"),
        "status": v.pointer("/status"),
    });
    let format = if output == "table" { "yaml" } else { output };
    crate::output::print_serialized(format, &view)
}

pub async fn create(client: &GryviaClient, k: &KindSpec, file: &str, next: &str) -> Result<()> {
    let text = std::fs::read_to_string(file).with_context(|| format!("Failed to read {file}"))?;
    let (objs, skipped) = objects_from_yaml(k, &text)?;
    for s in skipped {
        display::print_info(&format!("Skipped {s} (apply it with kubectl)"));
    }
    let api = api(client, k);
    for mut o in objs {
        if let Some(meta) = o.get_mut("metadata").and_then(Value::as_object_mut) {
            meta.remove("namespace");
        }
        let name = text_at(&o, "/metadata/name");
        let obj: DynamicObject = serde_json::from_value(o)?;
        api.create(&PostParams::default(), &obj)
            .await
            .map_err(|e| api_error(&format!("Failed to create {} '{name}'", k.noun), e))?;
        display::print_success(&format!("{} {name} created; {next}", capitalize(k.noun)));
    }
    Ok(())
}

pub async fn delete(
    client: &GryviaClient,
    k: &KindSpec,
    name: &str,
    yes: bool,
    consequence: &str,
) -> Result<()> {
    if !yes {
        let confirm = Confirm::new()
            .with_prompt(format!("Delete {} '{name}'? {consequence}", k.noun))
            .interact()?;
        if !confirm {
            display::print_info("Cancelled");
            return Ok(());
        }
    }
    api(client, k)
        .delete(name, &DeleteParams::default())
        .await
        .map_err(|e| api_error(&format!("Failed to delete {} '{name}'", k.noun), e))?;
    display::print_success(&format!("{} {name} deleted", capitalize(k.noun)));
    Ok(())
}

fn capitalize(s: &str) -> String {
    let mut c = s.chars();
    match c.next() {
        Some(f) => f.to_uppercase().collect::<String>() + c.as_str(),
        None => String::new(),
    }
}

/// Marker for the usual phase and state names of Gryvia kinds.
pub fn state_marker(state: &str) -> Marker {
    match state.to_ascii_lowercase().as_str() {
        "ready" | "running" | "serving" | "succeeded" => Marker::Ok,
        "syncing" | "pending" | "ingesting" | "deploying" | "progressing" => Marker::Warn,
        "error" | "failed" => Marker::Error,
        "suspended" => Marker::Disabled,
        _ => Marker::Unknown,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const COLS: &[Column] = &[
        Column {
            header: "NAME",
            ptr: "/metadata/name",
            marker: false,
        },
        Column {
            header: "STATE",
            ptr: "/status/state",
            marker: true,
        },
        Column {
            header: "FILES",
            ptr: "/status/fileCount",
            marker: false,
        },
    ];
    const K: KindSpec = KindSpec {
        kind: "GryviaDataset",
        plural: "gryviadatasets",
        cluster: true,
        noun: "dataset",
        title: "Datasets",
        columns: COLS,
        marker: state_marker,
    };

    #[test]
    fn table() {
        let items = vec![
            json!({"metadata": {"name": "corpus"}, "status": {"state": "ready", "fileCount": 3}}),
            json!({"metadata": {"name": "new"}}),
        ];
        let text = list_lines(&K, &items, false).join("\n");
        let expected = "\
━━━ Datasets ━━━

NAME    STATE  FILES
corpus  ready  3
new     -      -

Total: 2 datasets";
        assert_eq!(text, expected);
        assert!(list_lines(&K, &[], false)
            .join("\n")
            .contains("No datasets found"));
    }

    #[test]
    fn yaml_filters_kind() {
        let text = "kind: Secret\nmetadata: {name: s}\n---\nkind: GryviaDataset\nmetadata: {name: d}\nspec: {}\n";
        let (objs, skipped) = objects_from_yaml(&K, text).unwrap();
        assert_eq!(objs.len(), 1);
        assert_eq!(skipped, vec!["Secret s".to_string()]);
        assert!(objects_from_yaml(&K, "kind: Secret\nmetadata: {name: s}\n").is_err());
        assert!(objects_from_yaml(&K, "kind: GryviaDataset\nmetadata: {}\n").is_err());
    }

    #[test]
    fn markers() {
        assert_eq!(state_marker("Ready"), Marker::Ok);
        assert_eq!(state_marker("syncing"), Marker::Warn);
        assert_eq!(state_marker("Failed"), Marker::Error);
        assert_eq!(state_marker("x"), Marker::Unknown);
    }
}
