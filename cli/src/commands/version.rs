// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia version`: the client version plus the image versions of the platform's workloads.

use anyhow::Result;
use colored::Colorize;
use k8s_openapi::api::apps::v1::{DaemonSet, Deployment};
use kube::api::{Api, ListParams};

use crate::client::GryviaClient;
use crate::ui;

/// The CLI's own version line.
pub fn client_line() -> String {
    format!("gryvia {}", env!("CARGO_PKG_VERSION"))
}

pub fn print_client() {
    println!("{} {}", "Client:".bold(), client_line());
}

/// `name image` for each container image of a workload, as `(workload, image)` pairs.
pub fn images_of(name: &str, containers: &[Option<String>]) -> Vec<(String, String)> {
    containers
        .iter()
        .flatten()
        .map(|image| (name.to_string(), image.clone()))
        .collect()
}

pub async fn execute(client: &GryviaClient, namespace: &str) -> Result<()> {
    print_client();

    let mut rows: Vec<(String, String, String)> = Vec::new();
    let deployments: Api<Deployment> = Api::namespaced(client.kube_client.clone(), namespace);
    match deployments.list(&ListParams::default()).await {
        Ok(list) => {
            for d in list.items {
                let name = d.metadata.name.clone().unwrap_or_default();
                let images: Vec<Option<String>> = d
                    .spec
                    .and_then(|s| s.template.spec)
                    .map(|p| p.containers.into_iter().map(|c| c.image).collect())
                    .unwrap_or_default();
                for (n, i) in images_of(&name, &images) {
                    rows.push(("Deployment".into(), n, i));
                }
            }
        }
        Err(e) => {
            println!(
                "{} {}",
                ui::Marker::Warn.mark(),
                format!("Could not read the platform in namespace {namespace}: {e}").yellow()
            );
            return Ok(());
        }
    }
    let daemonsets: Api<DaemonSet> = Api::namespaced(client.kube_client.clone(), namespace);
    if let Ok(list) = daemonsets.list(&ListParams::default()).await {
        for d in list.items {
            let name = d.metadata.name.clone().unwrap_or_default();
            let images: Vec<Option<String>> = d
                .spec
                .and_then(|s| s.template.spec)
                .map(|p| p.containers.into_iter().map(|c| c.image).collect())
                .unwrap_or_default();
            for (n, i) in images_of(&name, &images) {
                rows.push(("DaemonSet".into(), n, i));
            }
        }
    }

    println!("{} {}", "Platform:".bold(), namespace);
    if rows.is_empty() {
        println!(
            "  {}",
            "No workloads found. Is Gryvia installed in this namespace?".dimmed()
        );
        return Ok(());
    }
    rows.sort();
    let kind_w = rows.iter().map(|r| r.0.len()).max().unwrap_or(0) + 2;
    let name_w = rows.iter().map(|r| r.1.len()).max().unwrap_or(0) + 2;
    for (kind, name, image) in rows {
        println!(
            "  {}{}{}",
            format!("{kind:<kind_w$}").dimmed(),
            format!("{name:<name_w$}").bold(),
            image
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn client_line_has_the_crate_version() {
        assert_eq!(
            client_line(),
            format!("gryvia {}", env!("CARGO_PKG_VERSION"))
        );
    }

    #[test]
    fn images_of_skips_containers_without_an_image() {
        let imgs = images_of("gw", &[Some("a:1".into()), None, Some("b:2".into())]);
        assert_eq!(
            imgs,
            vec![("gw".into(), "a:1".into()), ("gw".into(), "b:2".into())]
        );
    }
}
