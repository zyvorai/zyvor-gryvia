//! `gryvia ledger`: billing ledger verify / invoice finalize & void via the gateway.

use anyhow::{anyhow, Result};
use serde_json::json;

use crate::gateway::GatewayClient;
use crate::output;

pub async fn verify(gateway: &GatewayClient, tenant: &str, output: &str) -> Result<()> {
    let path = format!("/api/ledger/{}/verify", enc(tenant));
    let v = gateway
        .get_json(&path)
        .await
        .map_err(|e| anyhow!("ledger verify: {e}"))?;
    if output != "table" {
        return output::print_serialized(output, &v);
    }
    let ok = v.get("ok").and_then(|x| x.as_bool()).unwrap_or(false);
    let entries = v
        .get("entries")
        .and_then(|x| x.as_u64())
        .or_else(|| v.get("count").and_then(|x| x.as_u64()))
        .unwrap_or(0);
    println!(
        "ledger {tenant}: {} · entries≈{entries}",
        if ok { "chain OK" } else { "CHAIN BROKEN" }
    );
    Ok(())
}

pub async fn finalize(
    gateway: &GatewayClient,
    tenant: &str,
    month: &str,
    output: &str,
) -> Result<()> {
    let path = format!("/api/invoices/{}/{}/finalize", enc(tenant), enc(month));
    let v = gateway
        .post_json(&path, &json!({}))
        .await
        .map_err(|e| anyhow!("invoice finalize: {e}"))?;
    output::print_serialized(if output == "table" { "json" } else { output }, &v)
}

pub async fn void_invoice(
    gateway: &GatewayClient,
    tenant: &str,
    month: &str,
    reason: &str,
    output: &str,
) -> Result<()> {
    let path = format!("/api/invoices/{}/{}/void", enc(tenant), enc(month));
    let body = json!({ "reason": reason });
    let v = gateway
        .post_json(&path, &body)
        .await
        .map_err(|e| anyhow!("invoice void: {e}"))?;
    output::print_serialized(if output == "table" { "json" } else { output }, &v)
}

fn enc(s: &str) -> String {
    s.bytes()
        .map(|b| match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' => (b as char).to_string(),
            _ => format!("%{b:02X}"),
        })
        .collect()
}
