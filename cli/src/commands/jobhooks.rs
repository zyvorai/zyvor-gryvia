// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia job-hooks list|get|create|delete`: GryviaJobHook objects (namespaced). The ai-operator (with
//! --enable-job-hooks) POSTs a signed body or a Slack message when a GryviaAIJob or GryviaWorkflow in the hook's
//! namespace reaches one of its events (see docs/job-hooks.md).

use crate::commands::crd::{Column, KindSpec};
use crate::ui::Marker;

pub const JOB_HOOKS: KindSpec = KindSpec {
    kind: "GryviaJobHook",
    plural: "gryviajobhooks",
    cluster: false,
    noun: "job hook",
    title: "Job hooks",
    columns: &[
        Column {
            header: "NAME",
            ptr: "/metadata/name",
            marker: false,
        },
        Column {
            header: "STATE",
            ptr: "/status/conditions/0/reason",
            marker: true,
        },
        Column {
            header: "EVENTS",
            ptr: "/spec/events",
            marker: false,
        },
        Column {
            header: "URL",
            ptr: "/spec/webhook/url",
            marker: false,
        },
        Column {
            header: "DELIVERED",
            ptr: "/status/deliveries",
            marker: false,
        },
        Column {
            header: "FAILED",
            ptr: "/status/failures",
            marker: false,
        },
    ],
    marker: hook_marker,
};

/// Marker for the Ready condition's reason.
pub fn hook_marker(reason: &str) -> Marker {
    match reason {
        "Valid" => Marker::Ok,
        "Suspended" => Marker::Disabled,
        "InvalidSpec" | "SecretMissing" | "UnsupportedAPI" => Marker::Error,
        _ => Marker::Unknown,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::commands::crd::{list_lines, text_at};
    use serde_json::json;

    #[test]
    fn lists_events_and_counts() {
        let hook = json!({
            "metadata": {"name": "notify"},
            "spec": {"events": ["Failed", "Succeeded"], "webhook": {"url": "https://hooks.example.com/x"}},
            "status": {"conditions": [{"type": "Ready", "reason": "Valid"}], "deliveries": 3, "failures": 1},
        });
        assert_eq!(text_at(&hook, "/spec/events"), "Failed,Succeeded");
        let text = list_lines(&JOB_HOOKS, &[hook], false).join("\n");
        for want in [
            "notify",
            "Valid",
            "Failed,Succeeded",
            "https://hooks.example.com/x",
            "3",
            "1",
        ] {
            assert!(text.contains(want), "{want} missing from {text}");
        }
        assert!(matches!(hook_marker("Valid"), Marker::Ok));
        assert!(matches!(hook_marker("Suspended"), Marker::Disabled));
        assert!(matches!(hook_marker("SecretMissing"), Marker::Error));
        assert_eq!(text_at(&json!({"a": [1, 2]}), "/a"), "-");
    }
}
