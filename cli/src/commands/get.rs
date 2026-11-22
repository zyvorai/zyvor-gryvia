// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use kube::api::Api;

use crate::client::GryviaClient;
use crate::types::*;

pub async fn execute(
    client: &GryviaClient,
    resource: &str,
    name: &str,
    output: &str,
) -> Result<()> {
    // Validate output format
    match output {
        "json" | "yaml" => {}
        _ => anyhow::bail!(
            "Invalid output format: '{}'. Valid formats: json, yaml",
            output
        ),
    }

    match resource {
        "job" => get_job(client, name, output).await?,
        "quota" => get_quota(client, name, output).await?,
        "node" => get_node(client, name, output).await?,
        "experiment" => super::experiment::get(client, name, output).await?,
        _ => {
            anyhow::bail!(
                "Unknown resource type: {}. Valid types: job, quota, node, experiment",
                resource
            );
        }
    }

    Ok(())
}

async fn get_job(client: &GryviaClient, name: &str, output: &str) -> Result<()> {
    let api: Api<GryviaAIJob> = Api::namespaced(client.kube_client.clone(), client.namespace());

    let job = api.get(name).await.context("Failed to get job")?;

    match output {
        "json" => {
            println!("{}", serde_json::to_string_pretty(&job)?);
        }
        "yaml" => {
            println!("{}", serde_yaml::to_string(&job)?);
        }
        _ => {
            anyhow::bail!("Unknown output format '{}'. Use 'json' or 'yaml'.", output);
        }
    }

    Ok(())
}

async fn get_quota(client: &GryviaClient, name: &str, output: &str) -> Result<()> {
    let api: Api<GryviaQuota> = Api::all(client.kube_client.clone());

    let quota = api.get(name).await.context("Failed to get quota")?;

    match output {
        "json" => {
            println!("{}", serde_json::to_string_pretty(&quota)?);
        }
        "yaml" => {
            println!("{}", serde_yaml::to_string(&quota)?);
        }
        _ => {
            anyhow::bail!("Unknown output format '{}'. Use 'json' or 'yaml'.", output);
        }
    }

    Ok(())
}

async fn get_node(client: &GryviaClient, name: &str, output: &str) -> Result<()> {
    let api: Api<GryviaGpuNode> = Api::all(client.kube_client.clone());

    let node = api.get(name).await.context("Failed to get GPU node")?;

    match output {
        "json" => {
            println!("{}", serde_json::to_string_pretty(&node)?);
        }
        "yaml" => {
            println!("{}", serde_yaml::to_string(&node)?);
        }
        _ => {
            anyhow::bail!("Unknown output format '{}'. Use 'json' or 'yaml'.", output);
        }
    }

    Ok(())
}
