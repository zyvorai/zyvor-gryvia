// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia llm keys|models|usage`: the LLM gateway (docs/llm-gateway.md) through the Kubernetes API.
//!
//! A key is a Secret in the gateway's key namespace (default gryvia-llm-keys) labelled gryvia.io/llm-key=true that
//! holds only the sha256 of the key and the namespace it belongs to; `keys create` prints the key once. Models are
//! the GryviaInferenceServices annotated gryvia.io/llm-model; usage is the GryviaUsageRecords of kind tokens.

use std::collections::BTreeMap;

use anyhow::{anyhow, bail, Result};
use dialoguer::Confirm;
use k8s_openapi::api::core::v1::Secret;
use kube::api::{
    Api, ApiResource, DeleteParams, DynamicObject, GroupVersionKind, ListParams, ObjectMeta,
    PostParams,
};
use serde_json::{json, Value};

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

const KEY_LABEL: &str = "gryvia.io/llm-key";
const TENANT_LABEL: &str = "gryvia.io/tenant";
const NAME_LABEL: &str = "gryvia.io/llm-key-name";
const NAMESPACE_LABEL: &str = "gryvia.io/llm-key-namespace";
const PREFIX_ANNOTATION: &str = "gryvia.io/llm-key-prefix";
const DESCRIPTION_ANNOTATION: &str = "gryvia.io/description";

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

fn dynamic(client: &GryviaClient, kind: &str, plural: &str) -> Api<DynamicObject> {
    let gvk = GroupVersionKind::gvk("gryvia.io", "v1alpha1", kind);
    Api::all_with(
        client.kube_client.clone(),
        &ApiResource::from_gvk_with_plural(&gvk, plural),
    )
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// The stored form of a key: hex sha256.
pub fn hash_key(raw: &str) -> String {
    hex(ring::digest::digest(&ring::digest::SHA256, raw.as_bytes()).as_ref())
}

/// A new key: "gk-" and 32 random bytes in hex.
pub fn new_key() -> Result<String> {
    let mut buf = [0u8; 32];
    ring::rand::SecureRandom::fill(&ring::rand::SystemRandom::new(), &mut buf)
        .map_err(|_| anyhow!("no secure random source"))?;
    Ok(format!("gk-{}", hex(&buf)))
}

/// Tenant a key of this namespace is attributed to (tenant-<name> namespaces belong to tenant <name>).
pub fn tenant_of(namespace: &str) -> &str {
    namespace.strip_prefix("tenant-").unwrap_or(namespace)
}

/// The key Secret (without the key itself).
pub fn key_secret(
    key_ns: &str,
    namespace: &str,
    name: &str,
    raw: &str,
    description: &str,
) -> Secret {
    let mut annotations = BTreeMap::from([(PREFIX_ANNOTATION.to_string(), raw[..10].to_string())]);
    if !description.is_empty() {
        annotations.insert(DESCRIPTION_ANNOTATION.to_string(), description.to_string());
    }
    Secret {
        metadata: ObjectMeta {
            name: Some(format!("{namespace}.{name}")),
            namespace: Some(key_ns.to_string()),
            labels: Some(BTreeMap::from([
                (KEY_LABEL.to_string(), "true".to_string()),
                (TENANT_LABEL.to_string(), tenant_of(namespace).to_string()),
                (NAME_LABEL.to_string(), name.to_string()),
                (NAMESPACE_LABEL.to_string(), namespace.to_string()),
            ])),
            annotations: Some(annotations),
            ..Default::default()
        },
        type_: Some("Opaque".to_string()),
        string_data: Some(BTreeMap::from([
            ("hash".to_string(), hash_key(raw)),
            ("namespace".to_string(), namespace.to_string()),
        ])),
        ..Default::default()
    }
}

fn valid_name(name: &str) -> bool {
    let b = name.as_bytes();
    !b.is_empty()
        && b.len() <= 63
        && b.iter()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || *c == b'-')
        && b[0] != b'-'
        && b[b.len() - 1] != b'-'
}

fn keys_api(client: &GryviaClient, key_ns: &str) -> Api<Secret> {
    Api::namespaced(client.kube_client.clone(), key_ns)
}

pub async fn keys_create(
    client: &GryviaClient,
    key_ns: &str,
    name: &str,
    description: &str,
    output: &str,
) -> Result<()> {
    if !valid_name(name) {
        bail!("key name '{name}' must be lowercase letters, digits and '-' (at most 63)");
    }
    let namespace = client.namespace();
    let raw = new_key()?;
    keys_api(client, key_ns)
        .create(
            &PostParams::default(),
            &key_secret(key_ns, namespace, name, &raw, description),
        )
        .await
        .map_err(|e| api_error(&format!("Failed to create key '{name}' in {key_ns}"), e))?;
    if output != "table" {
        return crate::output::print_serialized(
            output,
            &json!({"name": name, "namespace": namespace, "tenant": tenant_of(namespace), "key": raw}),
        );
    }
    display::print_success(&format!(
        "Key {name} created for namespace {namespace}. Store it now, it is not shown again:"
    ));
    println!("{raw}");
    Ok(())
}

pub fn key_lines(secrets: &[Secret], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("LLM Keys", color), String::new()];
    if secrets.is_empty() {
        lines.push("No keys found".to_string());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = secrets
        .iter()
        .map(|s| {
            let labels = s.metadata.labels.clone().unwrap_or_default();
            let ann = s.metadata.annotations.clone().unwrap_or_default();
            let get = |m: &BTreeMap<String, String>, k: &str| {
                or_dash(m.get(k).map(String::as_str).unwrap_or(""))
            };
            vec![
                (get(&labels, NAME_LABEL), None),
                (get(&labels, NAMESPACE_LABEL), None),
                (get(&labels, TENANT_LABEL), None),
                (
                    format!(
                        "{}…",
                        ann.get(PREFIX_ANNOTATION).map(String::as_str).unwrap_or("")
                    ),
                    None,
                ),
                (get(&ann, DESCRIPTION_ANNOTATION), None),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &["NAME", "NAMESPACE", "TENANT", "PREFIX", "DESCRIPTION"],
        &rows,
        color,
    ));
    lines
}

async fn list_keys(client: &GryviaClient, key_ns: &str, all: bool) -> Result<Vec<Secret>> {
    let mut secrets = keys_api(client, key_ns)
        .list(&ListParams::default().labels(&format!("{KEY_LABEL}=true")))
        .await
        .map_err(|e| api_error(&format!("Failed to list keys in {key_ns}"), e))?
        .items;
    if !all {
        let ns = client.namespace();
        secrets.retain(|s| {
            s.metadata
                .labels
                .as_ref()
                .and_then(|l| l.get(NAMESPACE_LABEL))
                .is_some_and(|v| v == ns)
        });
    }
    secrets.sort_by_key(|s| s.metadata.name.clone());
    Ok(secrets)
}

pub async fn keys_list(client: &GryviaClient, key_ns: &str, all: bool, output: &str) -> Result<()> {
    let secrets = list_keys(client, key_ns, all).await?;
    if output != "table" {
        let items: Vec<Value> = secrets
            .iter()
            .map(|s| json!({"metadata": {"name": s.metadata.name, "labels": s.metadata.labels, "annotations": s.metadata.annotations}}))
            .collect();
        return crate::output::print_serialized(output, &items);
    }
    for line in key_lines(&secrets, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

pub async fn keys_delete(client: &GryviaClient, key_ns: &str, name: &str, yes: bool) -> Result<()> {
    let id = format!("{}.{name}", client.namespace());
    if !yes {
        let confirm = Confirm::new()
            .with_prompt(format!(
                "Revoke key '{name}' of namespace {}? Requests with it fail from now on",
                client.namespace()
            ))
            .interact()?;
        if !confirm {
            display::print_info("Cancelled");
            return Ok(());
        }
    }
    keys_api(client, key_ns)
        .delete(&id, &DeleteParams::default())
        .await
        .map_err(|e| api_error(&format!("Failed to revoke key '{name}'"), e))?;
    display::print_success(&format!("Key {name} revoked"));
    Ok(())
}

/// Published models (annotated services) visible from `namespace`: its own and the shared ones; all when None.
pub fn model_rows(services: &[Value], namespace: Option<&str>) -> Vec<Vec<Cell2>> {
    let mut rows: Vec<(String, String, Vec<Cell2>)> = services
        .iter()
        .filter_map(|s| {
            let model = str_at(s, "/metadata/annotations/gryvia.io~1llm-model").trim();
            if model.is_empty() {
                return None;
            }
            let ns = str_at(s, "/metadata/namespace");
            let shared = str_at(s, "/metadata/annotations/gryvia.io~1llm-shared") == "true";
            if namespace.is_some_and(|n| n != ns) && !shared {
                return None;
            }
            let ready = !str_at(s, "/status/endpoint").is_empty();
            let price = |k: &str| {
                or_dash(str_at(
                    s,
                    &format!("/metadata/annotations/gryvia.io~1llm-price-{k}-per-1m"),
                ))
            };
            Some((
                model.to_string(),
                ns.to_string(),
                vec![
                    (model.to_string(), None),
                    (ns.to_string(), None),
                    (str_at(s, "/metadata/name").to_string(), None),
                    (if shared { "yes" } else { "no" }.to_string(), None),
                    (
                        if ready { "Ready" } else { "NotReady" }.to_string(),
                        Some(if ready { Marker::Ok } else { Marker::Warn }),
                    ),
                    (price("input"), None),
                    (price("output"), None),
                ],
            ))
        })
        .collect();
    rows.sort_by(|a, b| (&a.0, &a.1).cmp(&(&b.0, &b.1)));
    rows.into_iter().map(|r| r.2).collect()
}

pub async fn models(client: &GryviaClient, all: bool, output: &str) -> Result<()> {
    let items = dynamic(client, "GryviaInferenceService", "gryviainferenceservices")
        .list(&ListParams::default())
        .await
        .map_err(|e| api_error("Failed to list inference services", e))?
        .items;
    let values: Vec<Value> = items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .collect();
    let rows = model_rows(&values, if all { None } else { Some(client.namespace()) });
    if output != "table" {
        let models: Vec<Value> = rows
            .iter()
            .map(|r| json!({"model": r[0].0, "namespace": r[1].0, "service": r[2].0, "shared": r[3].0 == "yes", "ready": r[4].0 == "Ready"}))
            .collect();
        return crate::output::print_serialized(output, &models);
    }
    let color = ui::color_enabled();
    println!("{}", ui::header("LLM Models", color));
    println!();
    if rows.is_empty() {
        println!(
            "No models published (annotate a GryviaInferenceService with gryvia.io/llm-model)"
        );
        return Ok(());
    }
    for line in ui::grid(
        &[
            "MODEL",
            "NAMESPACE",
            "SERVICE",
            "SHARED",
            "STATUS",
            "$/1M IN",
            "$/1M OUT",
        ],
        &rows,
        color,
    ) {
        println!("{line}");
    }
    Ok(())
}

/// Token records summed by `group_by` (model, tenant, namespace or day), only those starting at or after `since`
/// (an RFC 3339 prefix compare). Returns (key, input, output, cost) sorted by key.
pub fn summarize(records: &[Value], group_by: &str, since: &str) -> Vec<(String, i64, i64, f64)> {
    let mut groups: BTreeMap<String, (i64, i64, f64)> = BTreeMap::new();
    for r in records {
        if str_at(r, "/spec/kind") != "tokens" {
            continue;
        }
        let start = str_at(r, "/spec/start");
        if start < since {
            continue;
        }
        let key = match group_by {
            "tenant" => str_at(r, "/spec/tenant"),
            "namespace" => str_at(r, "/metadata/namespace"),
            "day" => start.get(..10).unwrap_or(start),
            _ => str_at(r, "/spec/model"),
        };
        let g = groups.entry(key.to_string()).or_default();
        g.0 += r
            .pointer("/spec/inputTokens")
            .and_then(Value::as_i64)
            .unwrap_or(0);
        g.1 += r
            .pointer("/spec/outputTokens")
            .and_then(Value::as_i64)
            .unwrap_or(0);
        g.2 += r
            .pointer("/spec/cost")
            .and_then(Value::as_f64)
            .unwrap_or(0.0);
    }
    groups
        .into_iter()
        .map(|(k, (i, o, c))| (k, i, o, c))
        .collect()
}

pub fn usage_lines(
    groups: &[(String, i64, i64, f64)],
    group_by: &str,
    days: u32,
    color: bool,
) -> Vec<String> {
    let mut lines = vec![
        ui::header(&format!("LLM Token Usage (last {days} days)"), color),
        String::new(),
    ];
    if groups.is_empty() {
        lines.push("No token usage recorded".to_string());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = groups
        .iter()
        .map(|(k, i, o, c)| {
            vec![
                (or_dash(k), None),
                (i.to_string(), None),
                (o.to_string(), None),
                (format!("{c:.4}"), None),
            ]
        })
        .collect();
    let head = group_by.to_uppercase();
    lines.extend(ui::grid(
        &[head.as_str(), "INPUT", "OUTPUT", "COST"],
        &rows,
        color,
    ));
    let (ti, to, tc) = groups
        .iter()
        .fold((0, 0, 0.0), |a, g| (a.0 + g.1, a.1 + g.2, a.2 + g.3));
    lines.push(String::new());
    lines.push(format!(
        "Total: {ti} input, {to} output tokens, cost {tc:.4}"
    ));
    lines
}

pub async fn usage(
    client: &GryviaClient,
    all: bool,
    group_by: &str,
    days: u32,
    output: &str,
) -> Result<()> {
    let gvk = GroupVersionKind::gvk("gryvia.io", "v1alpha1", "GryviaUsageRecord");
    let ar = ApiResource::from_gvk_with_plural(&gvk, "gryviausagerecords");
    let api: Api<DynamicObject> = if all {
        Api::all_with(client.kube_client.clone(), &ar)
    } else {
        Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar)
    };
    let items = api
        .list(&ListParams::default().labels("gryvia.io/usage-kind=tokens"))
        .await
        .map_err(|e| api_error("Failed to list usage records", e))?
        .items;
    let values: Vec<Value> = items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .collect();
    let since = (chrono::Utc::now().date_naive()
        - chrono::Duration::days(i64::from(days.max(1)) - 1))
    .format("%Y-%m-%d")
    .to_string();
    let groups = summarize(&values, group_by, &since);
    if output != "table" {
        let rows: Vec<Value> = groups
            .iter()
            .map(
                |(k, i, o, c)| json!({group_by: k, "inputTokens": i, "outputTokens": o, "cost": c}),
            )
            .collect();
        return crate::output::print_serialized(output, &rows);
    }
    for line in usage_lines(&groups, group_by, days, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn key_secret_holds_only_the_hash() {
        let raw = new_key().unwrap();
        assert!(raw.starts_with("gk-") && raw.len() == 67);
        assert_ne!(raw, new_key().unwrap());
        let s = key_secret("gryvia-llm-keys", "tenant-alpha", "ci", &raw, "CI runs");
        assert_eq!(s.metadata.name.as_deref(), Some("tenant-alpha.ci"));
        let labels = s.metadata.labels.unwrap();
        assert_eq!(labels[TENANT_LABEL], "alpha");
        assert_eq!(labels[KEY_LABEL], "true");
        let data = s.string_data.unwrap();
        assert_eq!(data["hash"], hash_key(&raw));
        assert_eq!(data["namespace"], "tenant-alpha");
        assert!(!format!("{data:?}").contains(&raw));
        assert_eq!(
            hash_key("abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }

    #[test]
    fn names() {
        assert!(valid_name("ci-runner-1"));
        assert!(!valid_name("CI"));
        assert!(!valid_name("-x"));
        assert!(!valid_name(""));
        assert!(!valid_name(&"a".repeat(64)));
    }

    fn svc(ns: &str, name: &str, ann: Value, endpoint: &str) -> Value {
        json!({"metadata": {"namespace": ns, "name": name, "annotations": ann}, "status": {"endpoint": endpoint}})
    }

    #[test]
    fn models_own_and_shared() {
        let services = vec![
            svc(
                "team-a",
                "chat",
                json!({"gryvia.io/llm-model": "chat", "gryvia.io/llm-price-input-per-1m": "0.5"}),
                "http://x",
            ),
            svc(
                "platform",
                "embed",
                json!({"gryvia.io/llm-model": "embed", "gryvia.io/llm-shared": "true"}),
                "",
            ),
            svc(
                "team-b",
                "other",
                json!({"gryvia.io/llm-model": "other"}),
                "http://y",
            ),
            svc("team-a", "plain", json!({}), "http://z"),
        ];
        let rows = model_rows(&services, Some("team-a"));
        let names: Vec<&str> = rows.iter().map(|r| r[0].0.as_str()).collect();
        assert_eq!(names, vec!["chat", "embed"]);
        assert_eq!(rows[0][5].0, "0.5");
        assert_eq!(rows[1][4].0, "NotReady");
        assert_eq!(model_rows(&services, None).len(), 3);
    }

    #[test]
    fn usage_summary() {
        let rec = |model: &str, start: &str, i: i64, o: i64, cost: f64| {
            json!({"metadata": {"namespace": "team-a"}, "spec": {"kind": "tokens", "tenant": "a", "model": model,
                   "start": start, "inputTokens": i, "outputTokens": o, "cost": cost}})
        };
        let records = vec![
            rec("chat", "2026-10-01T10:00:00Z", 100, 50, 0.25),
            rec("chat", "2026-10-01T11:00:00Z", 10, 5, 0.0),
            rec("embed", "2026-10-01T10:00:00Z", 7, 0, 0.0),
            rec("chat", "2026-09-01T10:00:00Z", 999, 999, 9.0),
            json!({"spec": {"job": "train", "start": "2026-10-01T10:00:00Z", "cost": 5.0}}),
        ];
        let groups = summarize(&records, "model", "2026-09-30");
        assert_eq!(
            groups,
            vec![
                ("chat".to_string(), 110, 55, 0.25),
                ("embed".to_string(), 7, 0, 0.0)
            ]
        );
        assert_eq!(summarize(&records, "day", "2026-01-01").len(), 2);
        let text = usage_lines(&groups, "model", 30, false).join("\n");
        assert!(text.contains("chat   110    55      0.2500"), "{text}");
        assert!(text.contains("Total: 117 input, 55 output tokens, cost 0.2500"));
    }
}
