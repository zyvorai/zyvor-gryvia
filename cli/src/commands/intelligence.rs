//! `gryvia intelligence`: call workload-intelligence analysis endpoints via the gateway.

use anyhow::{anyhow, Result};
use serde_json::{json, Value};

use crate::gateway::GatewayClient;
use crate::output;

const AREAS: &[&str] = &[
    "preflight",
    "scheduling",
    "bottlenecks",
    "cost",
    "inference-slo",
    "laboratory",
    "checkpoint",
    "dataset",
    "fabric",
    "capacity",
    "sovereign",
];

pub fn areas() -> &'static [&'static str] {
    AREAS
}

pub async fn schemas(gateway: &GatewayClient, output: &str) -> Result<()> {
    let v = gateway
        .get_json("/api/intelligence/schemas")
        .await
        .map_err(|e| anyhow!("intelligence schemas: {e}"))?;
    output::print_serialized(if output == "table" { "json" } else { output }, &v)
}

pub async fn analyze(
    gateway: &GatewayClient,
    area: &str,
    body: Value,
    output: &str,
) -> Result<()> {
    if !AREAS.contains(&area) {
        anyhow::bail!("unknown area {area}; choose one of: {}", AREAS.join(", "));
    }
    let path = format!("/api/intelligence/{area}");
    let v = gateway
        .post_json(&path, &body)
        .await
        .map_err(|e| anyhow!("intelligence {area}: {e}"))?;
    output::print_serialized(if output == "table" { "json" } else { output }, &v)
}

pub fn empty_body() -> Value {
    json!({})
}
