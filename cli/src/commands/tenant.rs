// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia tenant list|get|create|delete`: manage GryviaTenant objects (cluster-scoped) through the
//! Kubernetes API. The tenant controller creates the `tenant-<name>` namespace and its policies.

use anyhow::{anyhow, bail, Result};
use dialoguer::Confirm;
use kube::api::{DeleteParams, ListParams, Patch, PatchParams};
use serde_json::{json, Value};

use crate::client::GryviaClient;
use crate::display;
use crate::ui::{self, Cell2, Marker};

/// The namespace a tenant's workloads run in.
pub fn tenant_namespace(name: &str) -> String {
    format!("tenant-{name}")
}

/// A tenant name becomes part of a namespace (`tenant-<name>`), so it must be a DNS label of at most 56 characters.
pub fn validate_name(name: &str) -> Result<()> {
    let ok = !name.is_empty()
        && name.len() <= 56
        && name
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
        && !name.starts_with('-')
        && !name.ends_with('-');
    if ok {
        Ok(())
    } else {
        bail!(
            "invalid tenant name '{name}': use lowercase letters, digits and '-' (at most 56 characters, \
             starting and ending with a letter or digit)"
        )
    }
}

/// The GryviaTenant object `tenant create` applies. Only the fields that were given are set.
pub fn build_tenant(
    name: &str,
    display_name: Option<&str>,
    allowed_skus: &[String],
    max_gpus: Option<u32>,
    isolated: Option<bool>,
) -> Value {
    let mut spec = serde_json::Map::new();
    spec.insert("displayName".into(), json!(display_name.unwrap_or(name)));
    if !allowed_skus.is_empty() {
        spec.insert("allowedSkus".into(), json!(allowed_skus));
    }
    if let Some(n) = max_gpus {
        spec.insert("quotas".into(), json!({ "concurrentGPUs": n }));
    }
    if let Some(i) = isolated {
        spec.insert("networkPolicy".into(), json!({ "isolated": i }));
    }
    json!({
        "apiVersion": "gryvia.io/v1alpha1",
        "kind": "GryviaTenant",
        "metadata": { "name": name },
        "spec": Value::Object(spec),
    })
}

fn health_marker(health: &str) -> Marker {
    match health.trim().to_ascii_lowercase().as_str() {
        "healthy" => Marker::Ok,
        "warning" => Marker::Warn,
        "quota-exceeded" | "unhealthy" => Marker::Error,
        _ => Marker::Unknown,
    }
}

fn str_at<'a>(v: &'a Value, ptr: &str) -> &'a str {
    v.pointer(ptr).and_then(Value::as_str).unwrap_or("")
}

fn skus_of(v: &Value) -> Vec<String> {
    v.pointer("/spec/allowedSkus")
        .and_then(Value::as_array)
        .map(|a| {
            a.iter()
                .filter_map(|s| s.as_str().map(str::to_string))
                .collect()
        })
        .unwrap_or_default()
}

fn max_gpus_text(v: &Value) -> String {
    match v
        .pointer("/spec/quotas/concurrentGPUs")
        .and_then(Value::as_u64)
    {
        Some(n) if n > 0 => n.to_string(),
        _ => "unlimited".to_string(),
    }
}

fn isolated_of(v: &Value) -> Option<bool> {
    v.pointer("/spec/networkPolicy/isolated")
        .and_then(Value::as_bool)
}

/// The tenant table with a total line.
pub fn tenant_list_lines(tenants: &[Value], color: bool) -> Vec<String> {
    let mut lines = vec![ui::header("Tenants", color), String::new()];
    if tenants.is_empty() {
        lines.push("No tenants found".to_string());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = tenants
        .iter()
        .map(|t| {
            let name = str_at(t, "/metadata/name");
            let health = match str_at(t, "/status/health") {
                "" => "-",
                h => h,
            };
            let skus = skus_of(t);
            vec![
                (name.to_string(), None),
                (str_at(t, "/spec/displayName").to_string(), None),
                (health.to_string(), Some(health_marker(health))),
                (max_gpus_text(t), None),
                (
                    if skus.is_empty() {
                        "all".to_string()
                    } else {
                        skus.join(",")
                    },
                    None,
                ),
                (tenant_namespace(name), None),
            ]
        })
        .collect();
    lines.extend(ui::grid(
        &[
            "NAME",
            "DISPLAY NAME",
            "HEALTH",
            "MAX GPUs",
            "SKUs",
            "NAMESPACE",
        ],
        &rows,
        color,
    ));
    lines.push(String::new());
    lines.push(format!("Total: {} tenants", tenants.len()));
    lines
}

/// One tenant's detail report.
pub fn tenant_get_lines(t: &Value, color: bool) -> Vec<String> {
    const W: usize = 20;
    let name = str_at(t, "/metadata/name");
    let row = |k: &str, v: String| format!("  {}", ui::kv(k, &v, W, color));
    let health = match str_at(t, "/status/health") {
        "" => "-",
        h => h,
    };
    let skus = skus_of(t);
    let managed: Vec<&str> = t
        .pointer("/status/namespacesManaged")
        .and_then(Value::as_array)
        .map(|a| a.iter().filter_map(Value::as_str).collect())
        .unwrap_or_default();
    let mut lines = vec![
        ui::header(&format!("Tenant: {name}"), color),
        String::new(),
        ui::kv("Name", name, 8, color),
        ui::kv("Display name", str_at(t, "/spec/displayName"), 8, color),
        ui::kv(
            "Health",
            &health_marker(health).paint_with(health, color),
            8,
            color,
        ),
        String::new(),
        ui::section("Access", color),
        row("Namespace", tenant_namespace(name)),
        row(
            "Allowed SKUs",
            if skus.is_empty() {
                "all enabled SKUs".to_string()
            } else {
                skus.join(", ")
            },
        ),
        row("Max concurrent GPUs", max_gpus_text(t)),
        row(
            "Network isolated",
            match isolated_of(t) {
                Some(true) => "yes".to_string(),
                Some(false) => "no".to_string(),
                None => "default".to_string(),
            },
        ),
    ];
    if !managed.is_empty() {
        lines.push(row("Managed namespaces", managed.join(", ")));
    }
    lines
}

fn api(client: &GryviaClient) -> kube::Api<kube::core::DynamicObject> {
    super::capacity::gvk_api(client, "GryviaTenant", "gryviatenants")
}

fn api_error(what: &str, e: kube::Error) -> anyhow::Error {
    anyhow!("{what}: {}", display::cluster_error_text(&e.to_string()))
}

pub async fn list(client: &GryviaClient, output: &str) -> Result<()> {
    let items = api(client)
        .list(&ListParams::default())
        .await
        .map_err(|e| api_error("Failed to list tenants", e))?
        .items;
    if output != "table" {
        return crate::output::print_serialized(output, &items);
    }
    let values: Vec<Value> = items
        .iter()
        .filter_map(|o| serde_json::to_value(o).ok())
        .collect();
    for line in tenant_list_lines(&values, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

pub async fn get(client: &GryviaClient, name: &str, output: &str) -> Result<()> {
    let obj = api(client)
        .get(name)
        .await
        .map_err(|e| api_error(&format!("Failed to get tenant '{name}'"), e))?;
    if output != "table" {
        return crate::output::print_serialized(output, &obj);
    }
    let value = serde_json::to_value(&obj)?;
    for line in tenant_get_lines(&value, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

pub async fn create(
    client: &GryviaClient,
    name: &str,
    display_name: Option<&str>,
    allowed_skus: &[String],
    max_gpus: Option<u32>,
    isolated: Option<bool>,
) -> Result<()> {
    validate_name(name)?;
    let body = build_tenant(name, display_name, allowed_skus, max_gpus, isolated);
    let obj: kube::core::DynamicObject = serde_json::from_value(body)?;
    api(client)
        .patch(
            name,
            &PatchParams::apply("gryvia-cli").force(),
            &Patch::Apply(&obj),
        )
        .await
        .map_err(|e| api_error(&format!("Failed to create tenant '{name}'"), e))?;
    display::print_success(&format!(
        "Tenant {name} applied (namespace {})",
        tenant_namespace(name)
    ));
    Ok(())
}

pub async fn delete(client: &GryviaClient, name: &str, yes: bool) -> Result<()> {
    if !yes {
        let confirm = Confirm::new()
            .with_prompt(format!(
                "Delete tenant '{name}'? This can remove its namespace {} and everything in it",
                tenant_namespace(name)
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
        .map_err(|e| api_error(&format!("Failed to delete tenant '{name}'"), e))?;
    display::print_success(&format!("Tenant {name} deleted"));
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> Vec<Value> {
        vec![
            json!({"metadata": {"name": "acme"}, "spec": {"displayName": "Acme Corp", "allowedSkus": ["h100-8x", "a100"],
                   "quotas": {"concurrentGPUs": 16}, "networkPolicy": {"isolated": true}},
                   "status": {"health": "healthy", "namespacesManaged": ["tenant-acme"]}}),
            json!({"metadata": {"name": "beta"}, "spec": {"displayName": "Beta"}}),
        ]
    }

    #[test]
    fn build_tenant_sets_only_given_fields() {
        let t = build_tenant("acme", None, &[], None, None);
        assert_eq!(t["kind"], "GryviaTenant");
        assert_eq!(t["spec"]["displayName"], "acme");
        assert!(t["spec"].get("quotas").is_none());
        assert!(t["spec"].get("allowedSkus").is_none());
        assert!(t["spec"].get("networkPolicy").is_none());

        let t = build_tenant(
            "acme",
            Some("Acme Corp"),
            &["a100".to_string()],
            Some(8),
            Some(false),
        );
        assert_eq!(t["spec"]["displayName"], "Acme Corp");
        assert_eq!(t["spec"]["allowedSkus"], json!(["a100"]));
        assert_eq!(t["spec"]["quotas"]["concurrentGPUs"], 8);
        assert_eq!(t["spec"]["networkPolicy"]["isolated"], false);
    }

    #[test]
    fn names_must_be_namespace_safe() {
        assert!(validate_name("acme-1").is_ok());
        for bad in ["", "Acme", "-a", "a-", "a_b", &"x".repeat(57)] {
            assert!(validate_name(bad).is_err(), "{bad:?} should be rejected");
        }
        assert_eq!(tenant_namespace("acme"), "tenant-acme");
    }

    #[test]
    fn list_report_aligned() {
        let text = tenant_list_lines(&fixture(), false).join("\n");
        let expected = "\
━━━ Tenants ━━━

NAME  DISPLAY NAME  HEALTH   MAX GPUs   SKUs          NAMESPACE
acme  Acme Corp     healthy  16         h100-8x,a100  tenant-acme
beta  Beta          -        unlimited  all           tenant-beta

Total: 2 tenants";
        assert_eq!(text, expected);
    }

    #[test]
    fn empty_list() {
        assert!(tenant_list_lines(&[], false)
            .join("\n")
            .contains("No tenants found"));
    }

    #[test]
    fn get_report_lists_access_settings() {
        let text = tenant_get_lines(&fixture()[0], false).join("\n");
        assert!(text.contains("Tenant: acme"));
        assert!(text.contains("h100-8x, a100"));
        assert!(text.contains("Network isolated"));
        assert!(text.contains("Managed namespaces  tenant-acme"));
        let beta = tenant_get_lines(&fixture()[1], false).join("\n");
        assert!(beta.contains("all enabled SKUs"));
        assert!(beta.contains("unlimited"));
    }
}
