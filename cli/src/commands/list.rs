// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use kube::api::{Api, ListParams};
use serde_json;

use crate::client::GryviaClient;
use crate::display;
use crate::types::*;

pub async fn execute(
    client: &GryviaClient,
    resource: &str,
    all_namespaces: bool,
    output: &str,
) -> Result<()> {
    // Validate output format
    match output {
        "table" | "json" | "yaml" => {}
        _ => anyhow::bail!(
            "Invalid output format: '{}'. Valid formats: table, json, yaml",
            output
        ),
    }

    match resource {
        "jobs" | "job" => list_jobs(client, all_namespaces, output).await?,
        "quotas" | "quota" => {
            if all_namespaces {
                display::print_warning(
                    "--all-namespaces has no effect for cluster-scoped resource 'quotas'",
                );
            }
            list_quotas(client, output).await?
        }
        "nodes" | "node" => {
            if all_namespaces {
                display::print_warning(
                    "--all-namespaces has no effect for cluster-scoped resource 'nodes'",
                );
            }
            list_nodes(client, output).await?
        }
        "experiments" | "experiment" => {
            super::experiment::list(client, all_namespaces, output).await?
        }
        _ => {
            anyhow::bail!(
                "Unknown resource type: {}. Valid types: jobs, quotas, nodes, experiments",
                resource
            );
        }
    }

    Ok(())
}

async fn list_jobs(client: &GryviaClient, all_namespaces: bool, output: &str) -> Result<()> {
    let api: Api<GryviaAIJob> = if all_namespaces {
        Api::all(client.kube_client.clone())
    } else {
        Api::namespaced(client.kube_client.clone(), client.namespace())
    };

    let jobs = api
        .list(&ListParams::default())
        .await
        .context("Failed to list jobs")?;

    match output {
        "json" => {
            println!("{}", serde_json::to_string_pretty(&jobs)?);
        }
        "yaml" => {
            println!("{}", serde_yaml::to_string(&jobs)?);
        }
        _ => {
            display::print_jobs_table(&jobs.items);
        }
    }

    Ok(())
}

async fn list_quotas(client: &GryviaClient, output: &str) -> Result<()> {
    let api: Api<GryviaQuota> = Api::all(client.kube_client.clone());

    let quotas = api
        .list(&ListParams::default())
        .await
        .context("Failed to list quotas")?;

    match output {
        "json" => {
            println!("{}", serde_json::to_string_pretty(&quotas)?);
        }
        "yaml" => {
            println!("{}", serde_yaml::to_string(&quotas)?);
        }
        _ => {
            display::print_quotas_table(&quotas.items);
        }
    }

    Ok(())
}

async fn list_nodes(client: &GryviaClient, output: &str) -> Result<()> {
    let api: Api<GryviaGpuNode> = Api::all(client.kube_client.clone());

    let nodes = api
        .list(&ListParams::default())
        .await
        .context("Failed to list GPU nodes")?;

    match output {
        "json" => {
            println!("{}", serde_json::to_string_pretty(&nodes)?);
        }
        "yaml" => {
            println!("{}", serde_yaml::to_string(&nodes)?);
        }
        _ => {
            display::print_nodes_table(&nodes.items);
        }
    }

    Ok(())
}
