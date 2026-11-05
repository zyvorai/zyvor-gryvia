//! `gryvia flight`: Flight Recorder reports via the API gateway.

use anyhow::{anyhow, bail, Result};
use serde_json::Value;

use crate::gateway::GatewayClient;
use crate::output;

/// Fetch a job Flight Recorder report (and optional diagnosis).
pub async fn report(
    gateway: &GatewayClient,
    job: &str,
    namespace: Option<&str>,
    diagnosis: bool,
    require_complete: bool,
    output: &str,
) -> Result<()> {
    let mut path = format!("/api/flight/jobs/{}", urlencoding_path(job));
    let mut q: Vec<String> = Vec::new();
    if let Some(ns) = namespace {
        q.push(format!("namespace={}", urlencoding_path(ns)));
    }
    if !q.is_empty() {
        path.push('?');
        path.push_str(&q.join("&"));
    }
    let v = gateway
        .get_json(&path)
        .await
        .map_err(|e| anyhow!("flight report: {e}"))?;
    if require_complete {
        let complete = v
            .pointer("/coverage/complete")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        let truncated = v
            .pointer("/truncated")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        if !complete || truncated {
            bail!("flight report is partial or truncated (pass without --require-complete to print it)");
        }
    }
    if diagnosis {
        let mut dpath = format!("/api/flight/jobs/{}/diagnosis", urlencoding_path(job));
        if let Some(ns) = namespace {
            dpath.push_str(&format!("?namespace={}", urlencoding_path(ns)));
        }
        let d = gateway
            .get_json(&dpath)
            .await
            .map_err(|e| anyhow!("flight diagnosis: {e}"))?;
        let merged = serde_json::json!({ "report": v, "diagnosis": d });
        return output::print_serialized(output, &merged);
    }
    if output != "table" {
        return output::print_serialized(output, &v);
    }
    print_table(&v);
    Ok(())
}

fn urlencoding_path(s: &str) -> String {
    // job/ns names are DNS labels; keep simple percent-encoding for safety
    s.bytes()
        .map(|b| match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' => (b as char).to_string(),
            _ => format!("%{b:02X}"),
        })
        .collect()
}

fn print_table(v: &Value) {
    let job = v.get("job").and_then(Value::as_str).unwrap_or("?");
    let ns = v.get("namespace").and_then(Value::as_str).unwrap_or("?");
    let complete = v
        .pointer("/coverage/complete")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    let reachable = v.pointer("/coverage/reachable").and_then(Value::as_i64).unwrap_or(0);
    let total = v.pointer("/coverage/total").and_then(Value::as_i64).unwrap_or(0);
    let events = v
        .get("events")
        .and_then(Value::as_array)
        .map(|a| a.len())
        .unwrap_or(0);
    println!("Flight Recorder · job {job} · namespace {ns}");
    println!(
        "coverage: {reachable}/{total} collectors · complete={} · events={events}",
        if complete { "yes" } else { "no" }
    );
    if let Some(findings) = v.get("findings").and_then(Value::as_array) {
        for f in findings.iter().take(20) {
            let code = f.get("code").and_then(Value::as_str).unwrap_or("?");
            let node = f.get("node").and_then(Value::as_str).unwrap_or("?");
            let ev = f.get("evidence").and_then(Value::as_str).unwrap_or("");
            println!("  [{code}] {node}  {ev}");
        }
    }
}
