// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use dialoguer::Confirm;
use kube::api::{Api, ApiResource, DeleteParams, GroupVersionKind};
use kube::core::DynamicObject;

use crate::client::GryviaClient;
use crate::display;
use crate::types::*;

pub async fn execute(client: &GryviaClient, resource: &str, name: &str, yes: bool) -> Result<()> {
    if !yes {
        let confirm = Confirm::new()
            .with_prompt(format!("Delete {} '{}'?", resource, name))
            .interact()?;

        if !confirm {
            display::print_info("Cancelled");
            return Ok(());
        }
    }

    match resource {
        "job" => delete_job(client, name).await?,
        "quota" => delete_quota(client, name).await?,
        "storage" => delete_storage(client, name).await?,
        "network" => delete_network(client, name).await?,
        _ => {
            anyhow::bail!(
                "Unknown resource type: {}. Valid types: job, quota, storage, network",
                resource
            );
        }
    }

    Ok(())
}

async fn delete_job(client: &GryviaClient, name: &str) -> Result<()> {
    let api: Api<GryviaAIJob> = Api::namespaced(client.kube_client.clone(), client.namespace());

    api.delete(name, &DeleteParams::default())
        .await
        .context("Failed to delete job")?;

    display::print_success(&format!("Job {} deleted", name));

    Ok(())
}

async fn delete_quota(client: &GryviaClient, name: &str) -> Result<()> {
    let api: Api<GryviaQuota> = Api::all(client.kube_client.clone());

    api.delete(name, &DeleteParams::default())
        .await
        .context("Failed to delete quota")?;

    display::print_success(&format!("Quota {} deleted", name));

    Ok(())
}

async fn delete_storage(client: &GryviaClient, name: &str) -> Result<()> {
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaStorage",
    ));
    let api: Api<DynamicObject> = Api::all_with(client.kube_client.clone(), &ar);

    api.delete(name, &DeleteParams::default())
        .await
        .context("Failed to delete storage")?;

    display::print_success(&format!("Storage {} deleted", name));

    Ok(())
}

async fn delete_network(client: &GryviaClient, name: &str) -> Result<()> {
    let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
        "gryvia.io",
        "v1alpha1",
        "GryviaNetwork",
    ));
    let api: Api<DynamicObject> = Api::all_with(client.kube_client.clone(), &ar);

    api.delete(name, &DeleteParams::default())
        .await
        .context("Failed to delete network")?;

    display::print_success(&format!("Network {} deleted", name));

    Ok(())
}
