// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! Output formats shared by every command that can print structured data.

use anyhow::Result;
use clap::ValueEnum;
use serde::Serialize;

/// `-o/--output` for commands that print a table by default.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, ValueEnum)]
pub enum OutputFormat {
    /// Human-readable table (default)
    #[default]
    Table,
    /// JSON, for scripts
    Json,
    /// YAML
    Yaml,
}

impl OutputFormat {
    pub fn as_str(self) -> &'static str {
        match self {
            OutputFormat::Table => "table",
            OutputFormat::Json => "json",
            OutputFormat::Yaml => "yaml",
        }
    }
}

/// `-o/--output` for commands that only print an object (no table form), such as `get`.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, ValueEnum)]
pub enum StructuredFormat {
    /// YAML (default)
    #[default]
    Yaml,
    /// JSON
    Json,
}

impl StructuredFormat {
    pub fn as_str(self) -> &'static str {
        match self {
            StructuredFormat::Yaml => "yaml",
            StructuredFormat::Json => "json",
        }
    }
}

/// Print `value` as JSON, or as YAML when `format` is `"yaml"`.
pub fn print_serialized<T: Serialize + ?Sized>(format: &str, value: &T) -> Result<()> {
    if format == "yaml" {
        print!("{}", serde_yaml::to_string(value)?);
    } else {
        println!("{}", serde_json::to_string_pretty(value)?);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use clap::ValueEnum;

    #[test]
    fn formats_parse_from_their_names() {
        assert_eq!(
            OutputFormat::from_str("json", true).unwrap(),
            OutputFormat::Json
        );
        assert_eq!(
            OutputFormat::from_str("YAML", true).unwrap(),
            OutputFormat::Yaml
        );
        assert!(OutputFormat::from_str("xml", true).is_err());
        assert_eq!(StructuredFormat::default(), StructuredFormat::Yaml);
    }
}
