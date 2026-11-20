// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia agents list|get|create|delete|chat`: GryviaAgent objects (namespaced). The ai-operator (with
//! --enable-agents) runs each agent's tool-calling runtime; `chat` sends a message through the api-gateway's
//! POST /api/agents/{name}/chat, which proxies it to the agent (see docs/agents.md).

use std::time::Duration;

use anyhow::{bail, Result};
use serde_json::{json, Value};

use crate::commands::crd::{state_marker, Column, KindSpec};
use crate::gateway::{GatewayClient, GatewayConfig};
use crate::ui;

/// An agent call runs several model calls and tools; give it more than the default gateway timeout.
const CHAT_TIMEOUT: Duration = Duration::from_secs(180);

pub const AGENTS: KindSpec = KindSpec {
    kind: "GryviaAgent",
    plural: "gryviaagents",
    cluster: false,
    noun: "agent",
    title: "Agents",
    columns: &[
        Column {
            header: "NAME",
            ptr: "/metadata/name",
            marker: false,
        },
        Column {
            header: "MODEL",
            ptr: "/spec/model",
            marker: false,
        },
        Column {
            header: "PHASE",
            ptr: "/status/phase",
            marker: true,
        },
        Column {
            header: "READY",
            ptr: "/status/readyReplicas",
            marker: false,
        },
        Column {
            header: "ENDPOINT",
            ptr: "/status/endpoint",
            marker: false,
        },
    ],
    marker: state_marker,
};

pub fn chat_body(message: &str) -> Value {
    json!({"messages": [{"role": "user", "content": message}]})
}

pub fn reply_lines(agent: &str, reply: &Value, color: bool) -> Vec<String> {
    let content = reply
        .pointer("/choices/0/message/content")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let mut lines = vec![ui::header(agent, color), String::new()];
    lines.extend(content.lines().map(str::to_string));
    let calls = reply
        .pointer("/gryvia/toolCalls")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    let steps = reply
        .pointer("/gryvia/steps")
        .and_then(Value::as_i64)
        .unwrap_or(1);
    let mut summary = format!("{steps} model call{}", if steps == 1 { "" } else { "s" });
    if !calls.is_empty() {
        let tools: Vec<String> = calls
            .iter()
            .map(|c| {
                let name = c.get("tool").and_then(Value::as_str).unwrap_or("?");
                let ok = c.get("ok").and_then(Value::as_bool).unwrap_or(false);
                format!("{name} {}", if ok { "ok" } else { "failed" })
            })
            .collect();
        summary.push_str(&format!(", tools: {}", tools.join(", ")));
    }
    if let Some(t) = reply.pointer("/usage/total_tokens").and_then(Value::as_i64) {
        summary.push_str(&format!(", {t} tokens"));
    }
    if reply
        .pointer("/choices/0/finish_reason")
        .and_then(Value::as_str)
        == Some("length")
    {
        summary.push_str(" (stopped at maxSteps)");
    }
    lines.push(String::new());
    lines.push(summary);
    lines
}

/// Sends one user message to an agent through the api-gateway.
pub async fn chat(
    gateway: Option<GatewayConfig>,
    agent: &str,
    words: &[String],
    output: &str,
) -> Result<()> {
    let Some(mut cfg) = gateway else {
        bail!("gryvia agents chat goes through the api-gateway: set --gateway or GRYVIA_GATEWAY_URL (and GRYVIA_API_KEY)");
    };
    let message = words.join(" ");
    if message.trim().is_empty() {
        bail!("give a message, for example: gryvia agents chat {agent} what is the GPU quota");
    }
    cfg.timeout = cfg.timeout.max(CHAT_TIMEOUT);
    let reply = GatewayClient::new(cfg)
        .post_json(&format!("/api/agents/{agent}/chat"), &chat_body(&message))
        .await?;
    if output != "table" {
        return crate::output::print_serialized(output, &reply);
    }
    for line in reply_lines(agent, &reply, ui::color_enabled()) {
        println!("{line}");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::commands::crd;

    #[test]
    fn body() {
        assert_eq!(
            chat_body("hi there"),
            json!({"messages": [{"role": "user", "content": "hi there"}]})
        );
    }

    #[test]
    fn reply_summary() {
        let reply = json!({
            "choices": [{"message": {"content": "Quotas reset\nmonthly."}, "finish_reason": "stop"}],
            "usage": {"total_tokens": 42},
            "gryvia": {"steps": 2, "toolCalls": [{"tool": "search", "ok": true}, {"tool": "status", "ok": false}]}
        });
        let lines = reply_lines("helper", &reply, false).join("\n");
        assert!(lines.contains("Quotas reset\nmonthly."), "{lines}");
        assert!(
            lines.contains("2 model calls, tools: search ok, status failed, 42 tokens"),
            "{lines}"
        );
        let capped = json!({"choices": [{"message": {"content": "x"}, "finish_reason": "length"}], "gryvia": {"steps": 1}});
        let lines = reply_lines("helper", &capped, false).join("\n");
        assert!(
            lines.contains("1 model call (stopped at maxSteps)"),
            "{lines}"
        );
    }

    #[tokio::test]
    async fn chat_needs_a_gateway() {
        let err = chat(None, "helper", &["hi".to_string()], "table")
            .await
            .unwrap_err();
        assert!(err.to_string().contains("GRYVIA_GATEWAY_URL"), "{err}");
    }

    #[test]
    fn list_columns() {
        let item = json!({"metadata": {"name": "helper"}, "spec": {"model": "chat"},
            "status": {"phase": "Ready", "readyReplicas": 1, "endpoint": "http://helper-agent.t.svc.cluster.local:8080"}});
        let lines = crd::list_lines(&AGENTS, &[item], false).join("\n");
        for want in ["helper", "chat", "Ready", "helper-agent.t"] {
            assert!(lines.contains(want), "{want}: {lines}");
        }
    }
}
