// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

use anyhow::{Context, Result};
use kube::core::DynamicObject;
use serde_yaml;
use std::fs;

use crate::display;

pub async fn execute(file: &str) -> Result<()> {
    display::print_info(&format!("Validating: {}", file));

    let contents =
        fs::read_to_string(file).with_context(|| format!("Failed to read file: {}", file))?;

    let obj: DynamicObject =
        serde_yaml::from_str(&contents).context("Failed to parse YAML file")?;

    display::print_success("YAML file is valid");

    // Validate that it's a known Gryvia resource type
    let known_kinds = [
        "GryviaAIJob",
        "GryviaQuota",
        "GryviaGpuNode",
        "GryviaStorage",
        "GryviaNetwork",
    ];
    if let Some(ref types) = obj.types {
        if types.api_version != "gryvia.io/v1alpha1" {
            display::print_warning(&format!(
                "apiVersion '{}' is not gryvia.io/v1alpha1",
                types.api_version
            ));
        }
        if !known_kinds.contains(&types.kind.as_str()) {
            display::print_warning(&format!(
                "kind '{}' is not a known Gryvia resource type",
                types.kind
            ));
        } else {
            display::print_success(&format!("Resource type '{}' is valid", types.kind));
        }
    } else {
        display::print_warning("Missing apiVersion/kind in YAML");
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    fn write_temp(name: &str, body: &str) -> PathBuf {
        let path = std::env::temp_dir().join(format!(
            "gryvia-validate-{}-{}.yaml",
            std::process::id(),
            name
        ));
        fs::write(&path, body).unwrap();
        path
    }

    #[tokio::test]
    async fn accepts_a_known_resource() {
        let path = write_temp(
            "ok",
            "apiVersion: gryvia.io/v1alpha1\nkind: GryviaAIJob\nmetadata:\n  name: demo\n",
        );
        assert!(execute(path.to_str().unwrap()).await.is_ok());
        let _ = fs::remove_file(path);
    }

    #[tokio::test]
    async fn warns_but_succeeds_for_unknown_kinds() {
        let path = write_temp(
            "unknown",
            "apiVersion: gryvia.io/v1alpha1\nkind: Nope\nmetadata:\n  name: demo\n",
        );
        assert!(execute(path.to_str().unwrap()).await.is_ok());
        let _ = fs::remove_file(path);
    }

    #[tokio::test]
    async fn rejects_invalid_yaml_and_missing_files() {
        let bad = write_temp("bad", "metadata: [unclosed");
        assert!(execute(bad.to_str().unwrap()).await.is_err());
        let _ = fs::remove_file(bad);
        assert!(execute("/nonexistent/gryvia.yaml").await.is_err());
    }
}
