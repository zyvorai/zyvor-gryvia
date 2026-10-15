//! `gryvia workspace list|get|create|delete|new|pause|resume|open`: GryviaWorkspace objects (namespaced), the
//! Jupyter and VS Code environments the ai-operator runs. `open` asks the api-gateway for a short-lived launch link
//! (POST /api/workspaces/{name}/open, workspaceProxy in the Helm chart) and opens it in the browser.

use anyhow::{bail, Context, Result};
use kube::api::{Patch, PatchParams, PostParams};
use kube::core::DynamicObject;
use serde_json::{json, Value};

use crate::client::GryviaClient;
use crate::commands::crd::{self, api_error, state_marker, Column, KindSpec};
use crate::gateway::{GatewayClient, GatewayConfig};

pub const WORKSPACES: KindSpec = KindSpec {
    kind: "GryviaWorkspace",
    plural: "gryviaworkspaces",
    cluster: false,
    noun: "workspace",
    title: "Workspaces",
    columns: &[
        Column {
            header: "NAME",
            ptr: "/metadata/name",
            marker: false,
        },
        Column {
            header: "TYPE",
            ptr: "/spec/type",
            marker: false,
        },
        Column {
            header: "GPUS",
            ptr: "/spec/gpuCount",
            marker: false,
        },
        Column {
            header: "PHASE",
            ptr: "/status/phase",
            marker: true,
        },
        Column {
            header: "URL",
            ptr: "/status/url",
            marker: false,
        },
    ],
    marker: state_marker,
};

/// Flags of `gryvia workspace new`.
#[derive(Debug, Clone, Default)]
pub struct NewWorkspace {
    pub name: String,
    pub ws_type: String,
    pub gpus: u32,
    pub gpu_type: Option<String>,
    pub image: Option<String>,
    pub storage: Option<String>,
    pub idle_timeout_minutes: Option<u32>,
    pub idle_pause: bool,
}

pub fn build_workspace(w: &NewWorkspace, namespace: &str) -> Result<Value> {
    if !matches!(w.ws_type.as_str(), "jupyter" | "vscode") {
        bail!("--type is jupyter or vscode");
    }
    if w.idle_pause && w.idle_timeout_minutes.is_none() {
        bail!("--idle-pause needs --idle-timeout");
    }
    let mut spec = json!({ "type": w.ws_type });
    if w.gpus > 0 {
        spec["gpuCount"] = json!(w.gpus);
    }
    if let Some(t) = &w.gpu_type {
        spec["gpuType"] = json!(t);
    }
    if let Some(i) = &w.image {
        spec["image"] = json!(i);
    }
    if let Some(s) = &w.storage {
        spec["storage"] = json!(s);
    }
    if let Some(m) = w.idle_timeout_minutes {
        spec["idleTimeoutMinutes"] = json!(m);
    }
    let mut metadata = json!({ "name": w.name, "namespace": namespace });
    if w.idle_pause {
        metadata["annotations"] = json!({ "gryvia.io/idle-action": "pause" });
    }
    Ok(json!({
        "apiVersion": "gryvia.io/v1alpha1",
        "kind": "GryviaWorkspace",
        "metadata": metadata,
        "spec": spec,
    }))
}

pub async fn new(client: &GryviaClient, w: &NewWorkspace) -> Result<()> {
    let obj: DynamicObject = serde_json::from_value(build_workspace(w, client.namespace())?)
        .context("Invalid GryviaWorkspace")?;
    crd::api(client, &WORKSPACES)
        .create(&PostParams::default(), &obj)
        .await
        .map_err(|e| api_error(&format!("Failed to create workspace {}", w.name), e))?;
    println!(
        "Workspace {} created. When it is Running: gryvia workspace open {}",
        w.name, w.name
    );
    Ok(())
}

pub async fn set_paused(client: &GryviaClient, name: &str, paused: bool) -> Result<()> {
    let patch = json!({ "spec": { "paused": paused } });
    crd::api(client, &WORKSPACES)
        .patch(name, &PatchParams::default(), &Patch::Merge(&patch))
        .await
        .map_err(|e| api_error(&format!("Failed to update workspace {name}"), e))?;
    if paused {
        println!("Pausing workspace {name}; its storage is kept. Resume it with: gryvia workspace resume {name}");
    } else {
        println!("Resuming workspace {name}; open it when Running: gryvia workspace open {name}");
    }
    Ok(())
}

/// The launch URL from the gateway's reply, made absolute against the gateway when it is a path.
pub fn launch_url(base_url: &str, reply: &Value) -> Result<String> {
    let url = reply
        .get("url")
        .and_then(Value::as_str)
        .context("the gateway did not return a launch URL")?;
    if url.starts_with("https://") || url.starts_with("http://") {
        Ok(url.to_string())
    } else if url.starts_with('/') {
        Ok(format!("{}{}", base_url.trim_end_matches('/'), url))
    } else {
        bail!("unexpected launch URL from the gateway")
    }
}

pub async fn open(gateway: Option<GatewayConfig>, name: &str, print_only: bool) -> Result<()> {
    let Some(cfg) = gateway else {
        bail!("gryvia workspace open goes through the api-gateway: set --gateway or GRYVIA_GATEWAY_URL (and GRYVIA_API_KEY)");
    };
    let base = cfg.base_url.clone();
    let reply = GatewayClient::new(cfg)
        .post_json(&format!("/api/workspaces/{name}/open"), &json!({}))
        .await?;
    let url = launch_url(&base, &reply)?;
    if print_only || open_browser(&url).is_err() {
        println!("{url}");
        eprintln!("The link signs in whoever opens it within two minutes; do not share it.");
        return Ok(());
    }
    println!("Opened workspace {name} in your browser.");
    Ok(())
}

fn open_browser(url: &str) -> std::io::Result<()> {
    #[cfg(target_os = "macos")]
    let mut cmd = std::process::Command::new("open");
    #[cfg(target_os = "windows")]
    let mut cmd = {
        let mut c = std::process::Command::new("cmd");
        c.args(["/C", "start", ""]);
        c
    };
    #[cfg(all(not(target_os = "macos"), not(target_os = "windows")))]
    let mut cmd = std::process::Command::new("xdg-open");
    let status = cmd
        .arg(url)
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()?;
    if status.success() {
        Ok(())
    } else {
        Err(std::io::Error::other("browser opener failed"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ws(name: &str) -> NewWorkspace {
        NewWorkspace {
            name: name.into(),
            ws_type: "jupyter".into(),
            ..Default::default()
        }
    }

    #[test]
    fn workspace_matches_the_crd() {
        let crd: serde_yaml::Value = serde_yaml::from_str(include_str!(
            "../../../crds/gryvia.io_gryviaworkspaces.yaml"
        ))
        .unwrap();
        let props = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]
            ["properties"]
            .as_mapping()
            .unwrap();
        let mut w = ws("nb");
        w.gpus = 1;
        w.gpu_type = Some("A100".into());
        w.image = Some("my/nb:1".into());
        w.storage = Some("50Gi".into());
        w.idle_timeout_minutes = Some(60);
        w.idle_pause = true;
        let out = build_workspace(&w, "ml").unwrap();
        for key in out["spec"].as_object().unwrap().keys() {
            assert!(
                props.contains_key(key.as_str()),
                "spec.{key} is not in the CRD"
            );
        }
        assert_eq!(out["spec"]["gpuCount"], 1);
        assert_eq!(out["metadata"]["namespace"], "ml");
        assert_eq!(
            out["metadata"]["annotations"]["gryvia.io/idle-action"],
            "pause"
        );
    }

    #[test]
    fn cpu_workspace_is_minimal() {
        let out = build_workspace(&ws("nb"), "ml").unwrap();
        assert_eq!(out["spec"], json!({"type": "jupyter"}));
        assert!(out["metadata"].get("annotations").is_none());
    }

    #[test]
    fn rejects_bad_flags() {
        let mut w = ws("nb");
        w.ws_type = "emacs".into();
        assert!(build_workspace(&w, "ml").is_err());
        let mut w = ws("nb");
        w.idle_pause = true;
        assert!(build_workspace(&w, "ml").is_err());
    }

    #[test]
    fn launch_urls() {
        let abs = json!({"url": "https://g.example.com/ws/ml/nb/?gryvia_launch=t"});
        assert_eq!(
            launch_url("https://api.example.com", &abs).unwrap(),
            "https://g.example.com/ws/ml/nb/?gryvia_launch=t"
        );
        let rel = json!({"url": "/ws/ml/nb/?gryvia_launch=t"});
        assert_eq!(
            launch_url("https://localhost:8443/", &rel).unwrap(),
            "https://localhost:8443/ws/ml/nb/?gryvia_launch=t"
        );
        assert!(launch_url("https://x", &json!({"url": "javascript:alert(1)"})).is_err());
        assert!(launch_url("https://x", &json!({})).is_err());
    }

    #[tokio::test]
    async fn open_needs_a_gateway() {
        let err = open(None, "nb", true).await.unwrap_err();
        assert!(err.to_string().contains("GRYVIA_GATEWAY_URL"), "{err}");
    }
}
