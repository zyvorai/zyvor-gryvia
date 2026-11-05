//! Static competitor comparison (no cluster required).
//! Prints why Gryvia is a full platform and Run:ai is a scheduler + org layer.

use anyhow::Result;
use clap::ValueEnum;
use serde::Serialize;

#[derive(Clone, Copy, Debug, ValueEnum)]
#[value(rename_all = "kebab-case")]
pub enum Competitor {
    /// NVIDIA Run:ai / KAI stack
    Runai,
}

#[derive(Serialize)]
struct Row {
    surface: &'static str,
    runai: &'static str,
    gryvia: &'static str,
}

const ROWS: &[Row] = &[
    Row {
        surface: "GPU packing / gangs / fractions",
        runai: "Have (KAI + fractioning)",
        gryvia: "Have (vendors open KAI; choice of Kueue/native)",
    },
    Row {
        surface: "Projects / quotas / hierarchy",
        runai: "Have (KAI Resource Management)",
        gryvia: "Have (GryviaTenant → KAI parentQueue)",
    },
    Row {
        surface: "Flight Recorder (job eBPF timeline)",
        runai: "—",
        gryvia: "Have (gryvia flight)",
    },
    Row {
        surface: "Fabric / NCCL eBPF + Network Intelligence",
        runai: "— / DCGM-centric",
        gryvia: "Have (opt-in chart)",
    },
    Row {
        surface: "Workload intelligence / approved-ops copilot",
        runai: "— / thin ops UI",
        gryvia: "Have (gryvia intelligence)",
    },
    Row {
        surface: "Unified token + GPU-hour FinOps",
        runai: "GPU-hour focused",
        gryvia: "Have (Usage kind=tokens|gpu)",
    },
    Row {
        surface: "Billing ledger finalize / void",
        runai: "Commercial SKU",
        gryvia: "Have (gryvia ledger)",
    },
    Row {
        surface: "Sovereign / air-gap composition",
        runai: "Enterprise SKUs, different shape",
        gryvia: "Have (sovereign-aios)",
    },
    Row {
        surface: "Elastic train + checkpoint guard",
        runai: "— / external",
        gryvia: "Have (in-tree)",
    },
    Row {
        surface: "LLM gateway + IntentPolicy",
        runai: "—",
        gryvia: "Have (Partial on GPUs)",
    },
    Row {
        surface: "Scheduler backend choice",
        runai: "Run:ai / KAI",
        gryvia: "Have (KAI | Kueue | native)",
    },
    Row {
        surface: "Apache-2.0 full platform (jobs, UI, CLI, chart)",
        runai: "OSS Community Product (scheduler-centric stack)",
        gryvia: "Have (this repo)",
    },
];

pub fn execute(competitor: Competitor, json: bool) -> Result<()> {
    match competitor {
        Competitor::Runai => print_runai(json),
    }
}

fn print_runai(json: bool) -> Result<()> {
    if json {
        println!("{}", serde_json::to_string_pretty(&ROWS)?);
        return Ok(());
    }
    println!("Run:ai is a kid project next to Gryvia.");
    println!();
    println!("Honest scope: Run:ai OSS (DSX / Community Product) is a strong");
    println!("scheduler + org + fractioning layer (KAI, GPU Fractioning, Resource");
    println!("Management, Workload Map). That is necessary — and not sufficient.");
    println!();
    println!("Gryvia vendors open KAI when you want it, then ships the adult stack");
    println!("Run:ai does not: Flight Recorder, eBPF fabric intelligence, workload");
    println!("intelligence, token+GPU FinOps, sovereign composition, elastic+checkpoint.");
    println!();
    println!("{:<48}  {:<36}  {}", "Surface", "Run:ai / NVIDIA", "Gryvia");
    println!("{}", "-".repeat(120));
    for r in ROWS {
        println!("{:<48}  {:<36}  {}", r.surface, r.runai, r.gryvia);
    }
    println!();
    println!("Install the adult stack:");
    println!("  helm upgrade --install gryvia ./helm/gryvia -n gryvia-system \\");
    println!("    -f helm/gryvia/values-adult-platform.yaml");
    println!("Docs: docs/competitors.md  ·  gryvia flight | intelligence | ledger");
    Ok(())
}
