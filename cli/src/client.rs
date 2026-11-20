// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

use anyhow::{Context, Result};
use kube::{Client, Config};

pub struct GryviaClient {
    pub kube_client: Client,
    pub namespace: String,
}

impl GryviaClient {
    pub async fn new(context: Option<String>, namespace: Option<String>) -> Result<Self> {
        let config = if let Some(ctx) = context {
            Config::from_kubeconfig(&kube::config::KubeConfigOptions {
                context: Some(ctx),
                cluster: None,
                user: None,
            })
            .await
            .context("Failed to load kubeconfig")?
        } else {
            Config::infer()
                .await
                .context("Failed to infer Kubernetes config")?
        };

        let namespace = namespace.unwrap_or_else(|| config.default_namespace.clone());

        let mut config = config;
        config.connect_timeout = Some(std::time::Duration::from_secs(30));
        config.read_timeout = Some(std::time::Duration::from_secs(30));

        let kube_client = Client::try_from(config).context("Failed to create Kubernetes client")?;

        Ok(Self {
            kube_client,
            namespace,
        })
    }

    pub fn namespace(&self) -> &str {
        &self.namespace
    }
}
