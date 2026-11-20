// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia rag index|reingest|query`: GryviaVectorIndex objects (namespaced). The ai-operator (with --enable-rag)
//! keeps each index's store and runs an ingestion Job per dataset version; `query` asks the LLM gateway's
//! /v1/retrieve for the nearest chunks (see docs/rag.md).

use std::time::Duration;

use anyhow::{bail, Result};
use kube::api::{Patch, PatchParams};
use serde_json::{json, Value};

use crate::client::GryviaClient;
use crate::commands::crd::{self, api_error, state_marker, Column, KindSpec};
use crate::gateway::{GatewayClient, GatewayConfig};
use crate::ui::{self, Cell2};

pub const REINGEST_ANNOTATION: &str = "gryvia.io/reingest";

pub const VECTOR_INDEXES: KindSpec = KindSpec {
    kind: "GryviaVectorIndex",
    plural: "gryviavectorindexes",
    cluster: false,
    noun: "vector index",
    title: "Vector indexes",
    columns: &[
        Column {
            header: "NAME",
            ptr: "/metadata/name",
            marker: false,
        },
        Column {
            header: "DATASET",
            ptr: "/spec/datasetRef",
            marker: false,
        },
        Column {
            header: "MODEL",
            ptr: "/spec/embedding/model",
            marker: false,
        },
        Column {
            header: "STORE",
            ptr: "/spec/store/type",
            marker: false,
        },
        Column {
            header: "PHASE",
            ptr: "/status/phase",
            marker: true,
        },
        Column {
            header: "VERSION",
            ptr: "/status/datasetVersion",
            marker: false,
        },
        Column {
            header: "DOCS",
            ptr: "/status/documents",
            marker: false,
        },
        Column {
            header: "CHUNKS",
            ptr: "/status/chunks",
            marker: false,
        },
        Column {
            header: "INGESTED",
            ptr: "/status/lastIngested",
            marker: false,
        },
    ],
    marker: state_marker,
};

/// Sets the reingest annotation to a new value, which makes the controller run a fresh ingestion.
pub async fn reingest(client: &GryviaClient, name: &str) -> Result<()> {
    let idx = crd::get_value(client, &VECTOR_INDEXES, name).await?;
    if idx.pointer("/spec/suspend").and_then(Value::as_bool) == Some(true) {
        bail!("vector index {name} is suspended; set spec.suspend to false first");
    }
    let stamp = chrono::Utc::now()
        .timestamp_nanos_opt()
        .unwrap_or_default()
        .to_string();
    let patch = json!({"metadata": {"annotations": {REINGEST_ANNOTATION: stamp}}});
    crd::api(client, &VECTOR_INDEXES)
        .patch(name, &PatchParams::default(), &Patch::Merge(&patch))
        .await
        .map_err(|e| api_error(&format!("Failed to update vector index {name}"), e))?;
    println!("Re-ingesting vector index {name}; follow it with: gryvia rag index get {name}");
    Ok(())
}

pub fn retrieve_body(index: &str, query: &str, top_k: u32) -> Value {
    json!({"index": index, "query": query, "topK": top_k})
}

/// The LLM gateway to query: `--llm-gateway-url` / `GRYVIA_LLM_GATEWAY_URL` and a key of the index's namespace.
pub fn llm_gateway(url: Option<&str>, key: Option<&str>) -> Result<GatewayConfig> {
    let url = url.map(str::trim).unwrap_or_default().trim_end_matches('/');
    if url.is_empty() {
        bail!("set --llm-gateway-url or GRYVIA_LLM_GATEWAY_URL (for example after kubectl port-forward svc/gryvia-llm-gateway 8080 -n gryvia-system: http://localhost:8080)");
    }
    let key = key.map(str::trim).unwrap_or_default();
    if key.is_empty() {
        bail!("set --key or GRYVIA_LLM_KEY to an LLM gateway key of the index's namespace (gryvia llm keys create)");
    }
    Ok(GatewayConfig {
        base_url: url.to_string(),
        api_key: Some(key.to_string()),
        insecure: false,
        ca_file: std::env::var("GRYVIA_CA_FILE")
            .ok()
            .filter(|s| !s.trim().is_empty()),
        timeout: Duration::from_secs(60),
    })
}

fn one_line(s: &str, max: usize) -> String {
    let flat = s.split_whitespace().collect::<Vec<_>>().join(" ");
    if flat.chars().count() <= max {
        return flat;
    }
    flat.chars().take(max.saturating_sub(1)).collect::<String>() + "…"
}

pub fn hit_lines(index: &str, reply: &Value, color: bool) -> Vec<String> {
    let mut lines = vec![
        ui::header(&format!("Retrieved from {index}"), color),
        String::new(),
    ];
    let hits = reply
        .get("data")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    if hits.is_empty() {
        lines.push("No matching chunks".to_string());
        return lines;
    }
    let rows: Vec<Vec<Cell2>> = hits
        .iter()
        .map(|h| {
            let score = h.get("score").and_then(Value::as_f64).unwrap_or_default();
            let source = h.get("source").and_then(Value::as_str).unwrap_or("-");
            let chunk = h.get("chunk").and_then(Value::as_i64).unwrap_or_default();
            let text = h.get("text").and_then(Value::as_str).unwrap_or_default();
            vec![
                (format!("{score:.3}"), None),
                (format!("{source}#{chunk}"), None),
                (one_line(text, 100), None),
            ]
        })
        .collect();
    lines.extend(ui::grid(&["SCORE", "SOURCE", "TEXT"], &rows, color));
    if let Some(t) = reply
        .pointer("/usage/prompt_tokens")
        .and_then(Value::as_i64)
    {
        lines.push(String::new());
        lines.push(format!("{t} embedding tokens"));
    }
    lines
}

pub async fn query(
    cfg: GatewayConfig,
    index: &str,
    text: &str,
    top_k: u32,
    output: &str,
) -> Result<()> {
    let reply = GatewayClient::new(cfg)
        .post_json("/v1/retrieve", &retrieve_body(index, text, top_k))
        .await?;
    if output != "table" {
        return crate::output::print_serialized(output, &reply);
    }
    for line in hit_lines(index, &reply, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn body_and_gateway_config() {
        assert_eq!(
            retrieve_body("kb", "q", 3),
            json!({"index": "kb", "query": "q", "topK": 3})
        );
        let cfg = llm_gateway(Some("http://localhost:8080/"), Some(" gk-1 ")).unwrap();
        assert_eq!(cfg.base_url, "http://localhost:8080");
        assert_eq!(cfg.api_key.as_deref(), Some("gk-1"));
        assert!(llm_gateway(None, Some("k")).is_err());
        assert!(llm_gateway(Some("http://x"), None).is_err());
    }

    #[test]
    fn hits_table() {
        let reply = json!({"data": [
            {"score": 0.912, "source": "faq.md", "chunk": 2, "text": "Quotas   cap\nGPU hours."},
            {"score": 0.5, "source": "long.md", "chunk": 0, "text": "x".repeat(300)}
        ], "usage": {"prompt_tokens": 4}});
        let lines = hit_lines("kb", &reply, false).join("\n");
        assert!(lines.contains("0.912"), "{lines}");
        assert!(lines.contains("faq.md#2"), "{lines}");
        assert!(lines.contains("Quotas cap GPU hours."), "{lines}");
        assert!(lines.contains('…'), "{lines}");
        assert!(lines.contains("4 embedding tokens"), "{lines}");
        let empty = hit_lines("kb", &json!({"data": []}), false).join("\n");
        assert!(empty.contains("No matching chunks"));
    }

    #[test]
    fn list_columns() {
        let item = json!({"metadata": {"name": "kb"}, "spec": {"datasetRef": "docs", "embedding": {"model": "embed"},
            "store": {"type": "managed"}}, "status": {"phase": "Ready", "documents": 3, "chunks": 12}});
        let lines = crd::list_lines(&VECTOR_INDEXES, &[item], false).join("\n");
        for want in ["kb", "docs", "embed", "managed", "Ready", "12"] {
            assert!(lines.contains(want), "{want}: {lines}");
        }
    }
}
