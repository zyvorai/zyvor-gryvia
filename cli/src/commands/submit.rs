use anyhow::{Context, Result};
use colored::Colorize;
use futures::{AsyncBufReadExt, StreamExt};
use indicatif::{ProgressBar, ProgressStyle};
use k8s_openapi::api::core::v1::Pod;
use kube::api::{Api, ApiResource, ListParams, LogParams, PostParams};
use kube::core::DynamicObject;
use serde_json::json;
use serde_yaml;
use std::fs;
use std::path::Path;
use tokio::time::{sleep, Duration};

use super::sbatch;
use crate::client::GryviaClient;
use crate::display;

pub async fn execute(
    client: &GryviaClient,
    file: &str,
    wait: bool,
    follow_logs: bool,
) -> Result<()> {
    // Read and parse YAML file
    let contents =
        fs::read_to_string(file).with_context(|| format!("Failed to read file: {}", file))?;

    let job: DynamicObject =
        serde_yaml::from_str(&contents).context("Failed to parse YAML file")?;

    let job_name = job
        .metadata
        .name
        .clone()
        .unwrap_or_else(|| "unknown".to_string());

    display::print_info(&format!("Submitting job: {}", job_name));

    // Submit job
    let ar = ApiResource::from_gvk(&kube::api::GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaAIJob",
    ));
    let api: Api<DynamicObject> =
        Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar);

    let _result = api
        .create(&PostParams::default(), &job)
        .await
        .context("Failed to submit job")?;

    display::print_success(&format!("Job {} submitted successfully", job_name));

    if wait {
        display::print_info("Waiting for job to complete...");
        wait_for_completion(client, &job_name).await?;
    }

    if follow_logs {
        display::print_info("Waiting for pods to start...");
        follow_job_logs(client, &job_name).await?;
    }

    Ok(())
}

fn read_sbatch(
    script: &str,
    image: Option<String>,
    namespace: Option<String>,
) -> Result<sbatch::Imported> {
    let contents =
        fs::read_to_string(script).with_context(|| format!("Failed to read file: {}", script))?;
    let stem = Path::new(script)
        .file_stem()
        .and_then(|s| s.to_str())
        .unwrap_or("sbatch-job")
        .to_string();
    let out = sbatch::import(
        &contents,
        &sbatch::Options {
            image,
            default_name: stem,
            namespace,
        },
    )
    .with_context(|| format!("Cannot import {}", script))?;
    for w in &out.warnings {
        // stderr, so the --dry-run YAML on stdout can be piped to kubectl
        eprintln!("{} {}", "⚠".yellow().bold(), w);
    }
    Ok(out)
}

/// `gryvia submit --sbatch SCRIPT --dry-run`: the GryviaAIJobs as YAML on stdout, warnings on stderr.
pub fn print_sbatch(script: &str, image: Option<String>, namespace: Option<String>) -> Result<()> {
    let out = read_sbatch(script, image, namespace)?;
    for job in &out.jobs {
        print!("---\n{}", serde_yaml::to_string(job)?);
    }
    Ok(())
}

/// `gryvia submit --sbatch SCRIPT`: submits one GryviaAIJob (one per array index).
pub async fn execute_sbatch(
    client: &GryviaClient,
    script: &str,
    image: Option<String>,
    wait: bool,
    follow_logs: bool,
) -> Result<()> {
    let out = read_sbatch(script, image, Some(client.namespace().to_string()))?;
    let ar = ApiResource::from_gvk(&kube::api::GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaAIJob",
    ));
    let api: Api<DynamicObject> =
        Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar);
    let mut names = Vec::new();
    for job in out.jobs {
        let obj: DynamicObject = serde_json::from_value(job).context("Invalid GryviaAIJob")?;
        let name = obj.metadata.name.clone().unwrap_or_default();
        api.create(&PostParams::default(), &obj)
            .await
            .with_context(|| format!("Failed to submit job {}", name))?;
        display::print_success(&format!("Job {} submitted", name));
        names.push(name);
    }
    if wait {
        for name in &names {
            display::print_info(&format!("Waiting for {} to complete...", name));
            wait_for_completion(client, name).await?;
        }
    }
    if follow_logs {
        if names.len() > 1 {
            display::print_info("Following the logs of the first array job only");
        }
        follow_job_logs(client, &names[0]).await?;
    }
    Ok(())
}

/// Flags of the one-line `gryvia submit --image IMAGE [--gpu N] -- CMD...`.
#[derive(Debug, Clone, Default)]
pub struct InlineJob {
    pub image: String,
    pub name: Option<String>,
    pub job_type: String,
    pub gpu: Option<f64>,
    pub gpu_memory_mib: Option<u64>,
    pub isolation: Option<String>,
    pub gpu_type: Option<String>,
    pub mig: Option<String>,
    pub nodes: Option<u32>,
    pub queue: Option<String>,
    pub priority: Option<i32>,
    pub cpu: Option<String>,
    pub memory: Option<String>,
    pub env: Vec<String>,
    pub command: Vec<String>,
}

/// A job name prefix from the image: `ghcr.io/org/trainer:1.2` becomes `trainer`.
fn name_from_image(image: &str) -> String {
    let last = image.rsplit('/').next().unwrap_or(image);
    let base = last.split(['@', ':']).next().unwrap_or(last);
    let mut out: String = base
        .to_ascii_lowercase()
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() { c } else { '-' })
        .collect();
    out.truncate(40);
    let out = out.trim_matches('-').to_string();
    if out.is_empty() {
        "job".to_string()
    } else {
        out
    }
}

/// Builds the GryviaAIJob for a one-line submit. A fractional `--gpu` (below 1) becomes one GPU
/// shared through the `gryvia.io/gpu-share` annotation; whole numbers become `spec.gpus`.
pub fn build_inline_job(j: &InlineJob, namespace: &str) -> Result<serde_json::Value> {
    if j.image.trim().is_empty() {
        anyhow::bail!("--image must not be empty");
    }
    let mut annotations = serde_json::Map::new();
    let gpu = j.gpu.unwrap_or(if j.mig.is_some() { 1.0 } else { 0.0 });
    if !(gpu.is_finite() && gpu >= 0.0) {
        anyhow::bail!("--gpu must be zero or a positive number");
    }
    let fractional = gpu > 0.0 && gpu < 1.0;
    if gpu >= 1.0 && gpu.fract() != 0.0 {
        anyhow::bail!("--gpu takes a fraction below 1 (for example 0.5) or a whole number of GPUs");
    }
    if fractional && j.mig.is_some() {
        anyhow::bail!("--mig and a fractional --gpu are alternatives; use one");
    }
    if !fractional && (j.gpu_memory_mib.is_some() || j.isolation.is_some()) {
        anyhow::bail!(
            "--gpu-memory and --isolation apply to a fractional --gpu (for example --gpu 0.5)"
        );
    }
    let gpus: u32 = if fractional { 1 } else { gpu as u32 };
    if fractional {
        annotations.insert("gryvia.io/gpu-share".into(), json!(format!("{}", gpu)));
        if let Some(mem) = j.gpu_memory_mib {
            annotations.insert("gryvia.io/gpu-memory".into(), json!(mem.to_string()));
        }
        if let Some(iso) = &j.isolation {
            annotations.insert("gryvia.io/gpu-isolation".into(), json!(iso));
        }
    }

    let mut metadata = json!({
        "namespace": namespace,
        "labels": { "app.kubernetes.io/created-by": "gryvia-cli" },
    });
    match &j.name {
        Some(n) => metadata["name"] = json!(n),
        None => metadata["generateName"] = json!(format!("{}-", name_from_image(&j.image))),
    }
    if !annotations.is_empty() {
        metadata["annotations"] = serde_json::Value::Object(annotations);
    }

    let mut spec = json!({
        "type": if j.job_type.is_empty() { "training" } else { j.job_type.as_str() },
        "image": j.image,
        "gpus": gpus,
    });
    if !j.command.is_empty() {
        spec["command"] = json!(j.command);
    }
    if let Some(t) = &j.gpu_type {
        spec["gpuType"] = json!(t);
    }
    if let Some(m) = &j.mig {
        spec["migProfile"] = json!(m);
    }
    if let Some(q) = &j.queue {
        spec["queueName"] = json!(q);
    }
    if let Some(p) = j.priority {
        spec["priority"] = json!(p);
    }
    let mut requests = serde_json::Map::new();
    if let Some(c) = &j.cpu {
        requests.insert("cpu".into(), json!(c));
    }
    if let Some(m) = &j.memory {
        requests.insert("memory".into(), json!(m));
    }
    if !requests.is_empty() {
        spec["resources"] = json!({ "requests": requests });
    }
    if !j.env.is_empty() {
        let mut env = Vec::new();
        for kv in &j.env {
            let (k, v) = kv
                .split_once('=')
                .with_context(|| format!("-e {kv}: expected KEY=VALUE"))?;
            if k.is_empty() {
                anyhow::bail!("-e {kv}: empty variable name");
            }
            env.push(json!({ "name": k, "value": v }));
        }
        spec["env"] = json!(env);
    }
    if let Some(n) = j.nodes.filter(|n| *n > 1) {
        if fractional {
            anyhow::bail!("multi-node jobs need whole GPUs; drop the fractional --gpu");
        }
        spec["distributed"] = json!({
            "enabled": true,
            "framework": "pytorch",
            "nodes": n,
            "gpusPerNode": gpus,
        });
    }

    Ok(json!({
        "apiVersion": "gryvia.io/v1alpha1",
        "kind": "GryviaAIJob",
        "metadata": metadata,
        "spec": spec,
    }))
}

/// `gryvia submit --image ... --dry-run`: the GryviaAIJob as YAML on stdout.
pub fn print_inline(j: &InlineJob, namespace: &str) -> Result<()> {
    let job = build_inline_job(j, namespace)?;
    print!("{}", serde_yaml::to_string(&job)?);
    Ok(())
}

/// `gryvia submit --image IMAGE [flags] -- CMD...`: builds and submits one GryviaAIJob.
pub async fn execute_inline(
    client: &GryviaClient,
    j: &InlineJob,
    wait: bool,
    follow_logs: bool,
) -> Result<()> {
    let job = build_inline_job(j, client.namespace())?;
    if job["metadata"]["annotations"]["gryvia.io/gpu-share"].is_string() {
        eprintln!(
            "{} Fractional GPUs need the native queue scheduler or KAI Scheduler \
             (helm: aiOperator.nativeQueueScheduler=true or aiOperator.kaiScheduler=true); \
             without either the job gets a whole GPU.",
            "ℹ".cyan().bold()
        );
    }
    let obj: DynamicObject = serde_json::from_value(job).context("Invalid GryviaAIJob")?;
    let ar = ApiResource::from_gvk(&kube::api::GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaAIJob",
    ));
    let api: Api<DynamicObject> =
        Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar);
    let created = api
        .create(&PostParams::default(), &obj)
        .await
        .context("Failed to submit job")?;
    let name = created.metadata.name.unwrap_or_default();
    display::print_success(&format!("Job {} submitted", name));
    println!(
        "  {}  gryvia status {}\n  {}  gryvia logs {}",
        "status:".dimmed(),
        name,
        "logs:  ".dimmed(),
        name
    );
    if wait {
        display::print_info("Waiting for job to complete...");
        wait_for_completion(client, &name).await?;
    }
    if follow_logs {
        display::print_info("Waiting for pods to start...");
        follow_job_logs(client, &name).await?;
    }
    Ok(())
}

async fn wait_for_completion(client: &GryviaClient, job_name: &str) -> Result<()> {
    let spinner = ProgressBar::new_spinner();
    spinner.set_style(
        ProgressStyle::default_spinner()
            .template("{spinner:.green} {msg}")
            .expect("valid spinner template"),
    );

    let ar = ApiResource::from_gvk(&kube::api::GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaAIJob",
    ));
    let api: Api<DynamicObject> =
        Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar);

    let timeout = Duration::from_secs(24 * 60 * 60); // 24 hour timeout
    let start = tokio::time::Instant::now();

    loop {
        if start.elapsed() > timeout {
            spinner.finish_with_message(format!(
                "✗ Job {} timed out waiting for completion",
                job_name
            ));
            return Err(anyhow::anyhow!(
                "Timed out waiting for job {} to complete after 24h",
                job_name
            ));
        }

        spinner.set_message(format!("Checking status of {}", job_name));

        let job = api.get(job_name).await?;

        if let Some(status) = job.data.get("status") {
            if let Some(phase) = status.get("phase") {
                let phase_str = phase.as_str().unwrap_or("Unknown");

                match phase_str {
                    "Completed" | "Succeeded" => {
                        spinner.finish_with_message(format!("✓ Job {} completed", job_name));
                        return Ok(());
                    }
                    "Failed" => {
                        spinner.finish_with_message(format!("✗ Job {} failed", job_name));
                        let error_msg = status
                            .get("message")
                            .and_then(|m| m.as_str())
                            .unwrap_or("unknown error");
                        display::print_error(&format!("Error: {}", error_msg));
                        return Err(anyhow::anyhow!("Job {} failed: {}", job_name, error_msg));
                    }
                    _ => {
                        spinner.set_message(format!("Job status: {}", phase_str));
                    }
                }
            }
        }

        sleep(Duration::from_secs(5)).await;
    }
}

async fn follow_job_logs(client: &GryviaClient, job_name: &str) -> Result<()> {
    let pods_api: Api<Pod> = Api::namespaced(client.kube_client.clone(), client.namespace());

    let label_selector = format!("gryvia.io/job={}", job_name);
    let lp = ListParams::default().labels(&label_selector);

    // Wait for pods to appear (up to 5 minutes)
    let timeout = Duration::from_secs(300);
    let start = tokio::time::Instant::now();
    let pod_name;

    loop {
        if start.elapsed() > timeout {
            display::print_warning("Timed out waiting for pods to start");
            return Ok(());
        }

        let pods = pods_api.list(&lp).await?;
        if let Some(pod) = pods.items.first() {
            if let Some(name) = &pod.metadata.name {
                pod_name = name.clone();
                break;
            }
        }

        sleep(Duration::from_secs(3)).await;
    }

    display::print_info(&format!("Following logs from pod: {}", pod_name));

    // Detect container name from the pod spec, defaulting to "trainer"
    let pods = pods_api.list(&lp).await?;
    let container_name = pods
        .items
        .first()
        .and_then(|p| p.spec.as_ref())
        .and_then(|s| s.containers.first())
        .map(|c| c.name.clone())
        .unwrap_or_else(|| "trainer".to_string());

    let log_params = LogParams {
        follow: true,
        container: Some(container_name),
        ..Default::default()
    };

    let stream = pods_api
        .log_stream(&pod_name, &log_params)
        .await
        .context("Failed to start log stream")?;

    let mut lines = stream.lines();
    while let Some(line) = lines.next().await {
        match line {
            Ok(l) => println!("{}", l),
            Err(e) => {
                display::print_error(&format!("Log stream error: {}", e));
                break;
            }
        }
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn job(image: &str) -> InlineJob {
        InlineJob {
            image: image.into(),
            job_type: "training".into(),
            ..Default::default()
        }
    }

    /// Every spec field the one-line submit writes must exist in the CRD.
    fn assert_matches_crd(job: &serde_json::Value) {
        let crd: serde_yaml::Value =
            serde_yaml::from_str(include_str!("../../../crds/gryvia.io_gryviaaijobs.yaml"))
                .unwrap();
        let props = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]
            ["properties"]
            .as_mapping()
            .unwrap();
        for key in job["spec"].as_object().unwrap().keys() {
            assert!(
                props.contains_key(key.as_str()),
                "spec.{key} is not in the CRD"
            );
        }
    }

    #[test]
    fn name_comes_from_the_image() {
        assert_eq!(name_from_image("ghcr.io/org/my_trainer:1.2"), "my-trainer");
        assert_eq!(name_from_image("pytorch/pytorch@sha256:abc"), "pytorch");
        assert_eq!(name_from_image("busybox"), "busybox");
    }

    #[test]
    fn fractional_gpu_becomes_a_share_annotation() {
        let mut j = job("pytorch/pytorch:2.4.0");
        j.gpu = Some(0.5);
        j.gpu_memory_mib = Some(20480);
        j.command = vec!["python".into(), "train.py".into()];
        let out = build_inline_job(&j, "ml").unwrap();
        assert_matches_crd(&out);
        assert_eq!(out["spec"]["gpus"], 1);
        assert_eq!(out["metadata"]["generateName"], "pytorch-");
        assert_eq!(out["metadata"]["namespace"], "ml");
        let ann = &out["metadata"]["annotations"];
        assert_eq!(ann["gryvia.io/gpu-share"], "0.5");
        assert_eq!(ann["gryvia.io/gpu-memory"], "20480");
        assert_eq!(out["spec"]["command"], json!(["python", "train.py"]));
    }

    #[test]
    fn whole_gpus_and_nodes_make_a_distributed_job() {
        let mut j = job("img");
        j.name = Some("llama".into());
        j.gpu = Some(8.0);
        j.nodes = Some(2);
        j.gpu_type = Some("H100".into());
        j.queue = Some("research".into());
        j.priority = Some(10);
        j.cpu = Some("16".into());
        j.memory = Some("128Gi".into());
        j.env = vec!["NCCL_DEBUG=INFO".into()];
        let out = build_inline_job(&j, "ml").unwrap();
        assert_matches_crd(&out);
        assert_eq!(out["metadata"]["name"], "llama");
        assert!(out["metadata"].get("annotations").is_none());
        assert_eq!(out["spec"]["gpus"], 8);
        assert_eq!(out["spec"]["distributed"]["nodes"], 2);
        assert_eq!(out["spec"]["distributed"]["gpusPerNode"], 8);
        assert_eq!(out["spec"]["queueName"], "research");
        assert_eq!(out["spec"]["resources"]["requests"]["memory"], "128Gi");
        assert_eq!(out["spec"]["env"][0]["name"], "NCCL_DEBUG");
    }

    #[test]
    fn mig_defaults_to_one_slice() {
        let mut j = job("img");
        j.mig = Some("1g.10gb".into());
        let out = build_inline_job(&j, "ml").unwrap();
        assert_eq!(out["spec"]["gpus"], 1);
        assert_eq!(out["spec"]["migProfile"], "1g.10gb");
    }

    #[test]
    fn rejects_contradictory_flags() {
        let mut j = job("img");
        j.gpu = Some(1.5);
        assert!(build_inline_job(&j, "ml").is_err());

        let mut j = job("img");
        j.gpu = Some(2.0);
        j.gpu_memory_mib = Some(1024);
        assert!(build_inline_job(&j, "ml").is_err());

        let mut j = job("img");
        j.gpu = Some(0.5);
        j.nodes = Some(2);
        assert!(build_inline_job(&j, "ml").is_err());

        let mut j = job("img");
        j.env = vec!["NOEQUALS".into()];
        assert!(build_inline_job(&j, "ml").is_err());
    }

    #[test]
    fn cpu_only_by_default() {
        let out = build_inline_job(&job("busybox:1.36"), "ml").unwrap();
        assert_eq!(out["spec"]["gpus"], 0);
        assert!(out["spec"].get("command").is_none());
    }
}
