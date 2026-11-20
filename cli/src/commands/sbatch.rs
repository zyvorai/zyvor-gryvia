// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! `gryvia submit --sbatch`: turns a Slurm batch script into GryviaAIJobs.
//!
//! The `#SBATCH` directives become spec fields, the script runs as the container command, and the
//! `SLURM_*` variables a script usually reads are set from what the operator gives each pod (RANK,
//! WORLD_SIZE, MASTER_ADDR, JOB_COMPLETION_INDEX). `srun` runs its command once in each pod and
//! `scontrol show hostnames` prints the rendezvous host. Directives without an equivalent are
//! reported as warnings, never as errors. See docs/slurm.md.

use anyhow::{anyhow, bail, Result};
use serde_json::{json, Map, Value};

/// Most array indexes one script may expand to.
pub const MAX_ARRAY_JOBS: usize = 256;

#[derive(Debug, Default)]
pub struct Options {
    /// Container image; overrides `#SBATCH --container-image`.
    pub image: Option<String>,
    /// Job name when the script has no `--job-name` (usually the file name).
    pub default_name: String,
    /// Namespace written into the objects (None leaves it to the client's namespace).
    pub namespace: Option<String>,
}

#[derive(Debug)]
pub struct Imported {
    pub jobs: Vec<Value>,
    pub warnings: Vec<String>,
}

#[derive(Debug, Default)]
struct Directives {
    name: Option<String>,
    partition: Option<String>,
    nodes: Option<(u32, u32)>,
    ntasks: Option<u32>,
    ntasks_per_node: Option<u32>,
    gpus_per_node: Option<u32>,
    gpus_total: Option<u32>,
    gpus_per_task: Option<u32>,
    gpu_type: Option<String>,
    cpus_per_task: Option<u32>,
    cpus_per_gpu: Option<u32>,
    mem_mb: Option<u64>,
    mem_per_cpu_mb: Option<u64>,
    mem_per_gpu_mb: Option<u64>,
    time_secs: Option<u64>,
    array: Option<Vec<u32>>,
    chdir: Option<String>,
    image: Option<String>,
}

const BOOL_LONG: &[&str] = &[
    "exclusive",
    "requeue",
    "no-requeue",
    "hold",
    "overcommit",
    "contiguous",
    "spread-job",
    "use-min-nodes",
    "wait",
    "parsable",
    "test-only",
    "ignore-pbs",
    "oversubscribe",
    "quiet",
    "verbose",
    "no-kill",
    "kill-on-invalid-dep",
    "get-user-env",
    "container-writable",
    "container-remap-root",
    "no-container-mount-home",
];
const BOOL_SHORT: &[char] = &['H', 'O', 'Q', 'W', 'k', 's', 'v', 'h'];

/// The script's directives, in order, as (option, value) with short options spelled long.
fn directive_options(script: &str) -> Result<Vec<(String, Option<String>)>> {
    let mut out = Vec::new();
    for (i, line) in script.lines().enumerate() {
        let t = line.trim();
        if i == 0 && t.starts_with("#!") {
            continue;
        }
        if let Some(rest) = t.strip_prefix("#SBATCH") {
            if !rest.is_empty() && !rest.starts_with(char::is_whitespace) {
                continue;
            }
            let words = split_words(rest);
            let mut it = words.into_iter().peekable();
            while let Some(w) = it.next() {
                if let Some(long) = w.strip_prefix("--") {
                    match long.split_once('=') {
                        Some((k, v)) => out.push((k.to_string(), Some(v.to_string()))),
                        None if BOOL_LONG.contains(&long) => out.push((long.to_string(), None)),
                        None => {
                            let v = it.next().ok_or_else(|| {
                                anyhow!("line {}: --{} needs a value", i + 1, long)
                            })?;
                            out.push((long.to_string(), Some(v)));
                        }
                    }
                } else if let Some(short) = w.strip_prefix('-') {
                    let mut chars = short.chars();
                    let c = chars
                        .next()
                        .ok_or_else(|| anyhow!("line {}: empty option", i + 1))?;
                    let long = long_name(c).to_string();
                    if BOOL_SHORT.contains(&c) {
                        out.push((long, None));
                        continue;
                    }
                    let attached: String = chars.collect();
                    let v = if attached.is_empty() {
                        it.next()
                            .ok_or_else(|| anyhow!("line {}: -{} needs a value", i + 1, c))?
                    } else {
                        attached.trim_start_matches('=').to_string()
                    };
                    out.push((long, Some(v)));
                } else {
                    bail!("line {}: unexpected word {:?} in an #SBATCH line", i + 1, w);
                }
            }
        } else if t.is_empty() || t.starts_with('#') {
            continue;
        } else {
            break; // like sbatch: directives after the first command are ignored
        }
    }
    Ok(out)
}

fn long_name(c: char) -> &'static str {
    match c {
        'J' => "job-name",
        'p' => "partition",
        'N' => "nodes",
        'n' => "ntasks",
        't' => "time",
        'c' => "cpus-per-task",
        'G' => "gpus",
        'a' => "array",
        'o' => "output",
        'e' => "error",
        'D' => "chdir",
        'w' => "nodelist",
        'x' => "exclude",
        'C' => "constraint",
        'A' => "account",
        'q' => "qos",
        'd' => "dependency",
        'i' => "input",
        'm' => "distribution",
        'b' => "begin",
        'M' => "clusters",
        'L' => "licenses",
        'S' => "core-spec",
        'B' => "extra-node-info",
        'F' => "nodefile",
        'H' => "hold",
        'O' => "overcommit",
        'Q' => "quiet",
        'W' => "wait",
        'k' => "no-kill",
        's' => "oversubscribe",
        'v' => "verbose",
        'h' => "help",
        _ => "unknown",
    }
}

/// Splits on whitespace, honouring single and double quotes; a word starting with `#` ends the line.
fn split_words(s: &str) -> Vec<String> {
    let mut words = Vec::new();
    let mut cur = String::new();
    let mut quote: Option<char> = None;
    let mut in_word = false;
    for ch in s.chars() {
        match quote {
            Some(q) if ch == q => quote = None,
            Some(_) => cur.push(ch),
            None if ch == '"' || ch == '\'' => {
                quote = Some(ch);
                in_word = true;
            }
            None if ch.is_whitespace() => {
                if in_word {
                    words.push(std::mem::take(&mut cur));
                    in_word = false;
                }
            }
            None if ch == '#' && !in_word => break,
            None => {
                cur.push(ch);
                in_word = true;
            }
        }
    }
    if in_word {
        words.push(cur);
    }
    words
}

fn num(opt: &str, v: &str) -> Result<u32> {
    v.trim()
        .parse::<u32>()
        .map_err(|_| anyhow!("--{}={}: not a whole number", opt, v))
}

/// `N` or `min-max`.
fn parse_nodes(v: &str) -> Result<(u32, u32)> {
    let (lo, hi) = match v.split_once('-') {
        Some((a, b)) => (num("nodes", a)?, num("nodes", b)?),
        None => {
            let n = num("nodes", v)?;
            (n, n)
        }
    };
    if lo == 0 || hi < lo {
        bail!("--nodes={}: need 1 <= min <= max", v);
    }
    Ok((lo, hi))
}

type TypedCount = (Option<String>, u32);

/// `[type:]N` (N defaults to 1 for a bare type in --gres).
fn parse_typed_count(opt: &str, v: &str) -> Result<TypedCount> {
    match v.rsplit_once(':') {
        Some((t, n)) => Ok((Some(t.to_string()), num(opt, n)?)),
        None => match v.parse::<u32>() {
            Ok(n) => Ok((None, n)),
            Err(_) => Ok((Some(v.to_string()), 1)),
        },
    }
}

/// `--gres=gpu[:type][:N][,other...]`: the GPU part, and the names of the other resources.
fn parse_gres(v: &str) -> Result<(Option<TypedCount>, Vec<String>)> {
    let mut gpu = None;
    let mut others = Vec::new();
    for item in v.split(',') {
        let item = item.trim();
        let mut parts = item.split(':');
        let name = parts.next().unwrap_or_default();
        if name != "gpu" {
            if !item.is_empty() {
                others.push(item.to_string());
            }
            continue;
        }
        let rest: Vec<&str> = parts.collect();
        gpu = Some(match rest.as_slice() {
            [] => (None, 1),
            [n] if n.parse::<u32>().is_ok() => (None, num("gres", n)?),
            [t] => (Some(t.to_string()), 1),
            [t, n] => (Some(t.to_string()), num("gres", n)?),
            _ => bail!("--gres={}: expected gpu[:type][:count]", v),
        });
    }
    Ok((gpu, others))
}

/// The operator's GPU type names are upper case (H100, A100, L40); Slurm sites often write a100 or tesla_a100.
fn gpu_type_name(t: &str) -> String {
    let t = t.to_ascii_lowercase();
    let t = t.trim_start_matches("nvidia_").trim_start_matches("tesla_");
    t.to_ascii_uppercase()
}

/// Slurm memory: a number with an optional K/M/G/T suffix, megabytes by default.
fn parse_mem_mb(opt: &str, v: &str) -> Result<u64> {
    let v = v.trim();
    let (digits, unit) = match v.char_indices().find(|(_, c)| !c.is_ascii_digit()) {
        Some((i, _)) => (&v[..i], v[i..].to_ascii_uppercase()),
        None => (v, String::new()),
    };
    let n: u64 = digits
        .parse()
        .map_err(|_| anyhow!("--{}={}: not a size", opt, v))?;
    let mb = match unit.trim_end_matches('B') {
        "" | "M" => n,
        "K" => n.div_ceil(1024),
        "G" => n * 1024,
        "T" => n * 1024 * 1024,
        _ => bail!("--{}={}: unknown unit (K, M, G or T)", opt, v),
    };
    Ok(mb)
}

fn quantity_mb(mb: u64) -> String {
    if mb.is_multiple_of(1024 * 1024) {
        format!("{}Ti", mb / (1024 * 1024))
    } else if mb.is_multiple_of(1024) {
        format!("{}Gi", mb / 1024)
    } else {
        format!("{}Mi", mb)
    }
}

/// Slurm `--time`: minutes, MM:SS, HH:MM:SS, D-HH, D-HH:MM, D-HH:MM:SS; UNLIMITED/INFINITE is none.
fn parse_time(v: &str) -> Result<Option<u64>> {
    let v = v.trim();
    if v.eq_ignore_ascii_case("unlimited") || v.eq_ignore_ascii_case("infinite") {
        return Ok(None);
    }
    let bad = || {
        anyhow!(
            "--time={}: expected minutes, MM:SS, HH:MM:SS or D-HH[:MM[:SS]]",
            v
        )
    };
    let n = |s: &str| s.parse::<u64>().map_err(|_| bad());
    let (days, rest) = match v.split_once('-') {
        Some((d, r)) => (Some(n(d)?), r),
        None => (None, v),
    };
    let parts: Vec<&str> = rest.split(':').collect();
    let secs = match (days, parts.as_slice()) {
        (None, [m]) => n(m)? * 60,
        (None, [m, s]) => n(m)? * 60 + n(s)?,
        (None, [h, m, s]) => n(h)? * 3600 + n(m)? * 60 + n(s)?,
        (Some(d), [h]) => d * 86400 + n(h)? * 3600,
        (Some(d), [h, m]) => d * 86400 + n(h)? * 3600 + n(m)? * 60,
        (Some(d), [h, m, s]) => d * 86400 + n(h)? * 3600 + n(m)? * 60 + n(s)?,
        _ => return Err(bad()),
    };
    if secs == 0 {
        return Ok(None); // --time=0 is no limit in Slurm
    }
    Ok(Some(secs))
}

/// Seconds in the operator's timeout format, e.g. 1d2h30m.
fn timeout_string(secs: u64) -> String {
    let (d, rem) = (secs / 86400, secs % 86400);
    let (h, m, s) = (rem / 3600, rem % 3600 / 60, rem % 60);
    let mut out = String::new();
    for (n, u) in [(d, "d"), (h, "h"), (m, "m"), (s, "s")] {
        if n > 0 {
            out.push_str(&format!("{}{}", n, u));
        }
    }
    out
}

/// `0-3`, `1,3,5-7`, `0-15:4`, with an optional `%limit` suffix (returned separately).
fn parse_array(v: &str) -> Result<(Vec<u32>, Option<String>)> {
    let (spec, limit) = match v.split_once('%') {
        Some((s, l)) => (s, Some(l.to_string())),
        None => (v, None),
    };
    let mut ids = Vec::new();
    for part in spec.split(',') {
        let (range, step) = match part.split_once(':') {
            Some((r, s)) => (r, num("array", s)?),
            None => (part, 1),
        };
        if step == 0 {
            bail!("--array={}: step must be positive", v);
        }
        let (lo, hi) = match range.split_once('-') {
            Some((a, b)) => (num("array", a)?, num("array", b)?),
            None => {
                let n = num("array", range)?;
                (n, n)
            }
        };
        if hi < lo {
            bail!("--array={}: range {} runs backwards", v, range);
        }
        let mut i = lo;
        while i <= hi {
            if !ids.contains(&i) {
                ids.push(i);
            }
            if ids.len() > MAX_ARRAY_JOBS {
                bail!("--array={}: more than {} indexes", v, MAX_ARRAY_JOBS);
            }
            i += step;
        }
    }
    ids.sort_unstable();
    Ok((ids, limit))
}

/// A DNS-1123 label from a Slurm job name (lower case, `-` for anything else, at most `max` long).
pub fn k8s_name(s: &str, max: usize) -> String {
    let mut out = String::new();
    for ch in s.to_ascii_lowercase().chars() {
        if ch.is_ascii_alphanumeric() {
            out.push(ch);
        } else if !out.ends_with('-') {
            out.push('-');
        }
    }
    let out: String = out.trim_matches('-').chars().take(max).collect();
    let out = out.trim_end_matches('-').to_string();
    if out.is_empty() {
        "sbatch-job".to_string()
    } else {
        out
    }
}

fn parse_directives(script: &str, warnings: &mut Vec<String>) -> Result<Directives> {
    let mut d = Directives::default();
    for (opt, val) in directive_options(script)? {
        let v = val.clone().unwrap_or_default();
        match opt.as_str() {
            "job-name" => d.name = Some(v),
            "partition" => d.partition = Some(v),
            "nodes" => d.nodes = Some(parse_nodes(&v)?),
            "ntasks" => d.ntasks = Some(num(&opt, &v)?),
            "ntasks-per-node" => d.ntasks_per_node = Some(num(&opt, &v)?),
            "cpus-per-task" => d.cpus_per_task = Some(num(&opt, &v)?),
            "cpus-per-gpu" => d.cpus_per_gpu = Some(num(&opt, &v)?),
            "gres" => {
                let (gpu, others) = parse_gres(&v)?;
                if let Some((t, n)) = gpu {
                    d.gpus_per_node = Some(n);
                    d.gpu_type = t.or(d.gpu_type);
                }
                if !others.is_empty() {
                    warnings.push(format!("--gres {}: only GPUs are mapped; ignored", others.join(",")));
                }
            }
            "gpus-per-node" => {
                let (t, n) = parse_typed_count(&opt, &v)?;
                d.gpus_per_node = Some(n);
                d.gpu_type = t.or(d.gpu_type);
            }
            "gpus" => {
                let (t, n) = parse_typed_count(&opt, &v)?;
                d.gpus_total = Some(n);
                d.gpu_type = t.or(d.gpu_type);
            }
            "gpus-per-task" => {
                let (t, n) = parse_typed_count(&opt, &v)?;
                d.gpus_per_task = Some(n);
                d.gpu_type = t.or(d.gpu_type);
            }
            "mem" => {
                let mb = parse_mem_mb(&opt, &v)?;
                if mb == 0 {
                    warnings.push("--mem=0 (all of the node's memory) has no equivalent; no memory request is set".into());
                } else {
                    d.mem_mb = Some(mb);
                }
            }
            "mem-per-cpu" => d.mem_per_cpu_mb = Some(parse_mem_mb(&opt, &v)?),
            "mem-per-gpu" => d.mem_per_gpu_mb = Some(parse_mem_mb(&opt, &v)?),
            "time" => d.time_secs = parse_time(&v)?,
            "array" => {
                let (ids, limit) = parse_array(&v)?;
                if let Some(l) = limit {
                    warnings.push(format!("--array %{}: no concurrency limit is applied; every index is submitted at once (a Kueue queue can hold them)", l));
                }
                d.array = Some(ids);
            }
            "chdir" => d.chdir = Some(v),
            "container-image" => d.image = Some(pyxis_image(&v)),
            "output" | "error" | "open-mode" | "input" => {
                warnings.push(format!("--{}: ignored; read the output with `gryvia logs`", opt))
            }
            "dependency" => warnings.push("--dependency: ignored; chain jobs with a GryviaWorkflow".into()),
            "signal" => warnings.push(
                "--signal: ignored; pods get SIGTERM on eviction (see gryvia.io/checkpoint-command in docs/admission-recovery.md)".into(),
            ),
            "exclusive" => warnings.push("--exclusive: ignored; a pod gets its requested GPUs, CPUs and memory".into()),
            "export" | "get-user-env" => {
                warnings.push(format!("--{}: ignored; the job's environment is the image's plus the SLURM_* variables", opt))
            }
            "wait" | "parsable" | "quiet" | "verbose" => {
                warnings.push(format!("--{}: ignored; use `gryvia submit --wait`", opt))
            }
            other => warnings.push(match val {
                Some(v) => format!("--{}={}: no equivalent; ignored", other, v),
                None => format!("--{}: no equivalent; ignored", other),
            }),
        }
    }
    Ok(d)
}

/// pyxis writes `registry#image:tag`; the registry separator is `/` in an image reference.
fn pyxis_image(v: &str) -> String {
    v.replacen('#', "/", 1)
}

/// The interpreter of the shebang line: the command words and whether it is a shell.
fn interpreter(script: &str) -> Result<(Vec<String>, bool)> {
    let first = script.lines().next().unwrap_or_default().trim();
    let line = first.strip_prefix("#!").ok_or_else(|| {
        anyhow!("the script must start with #! and an interpreter, like sbatch requires")
    })?;
    let mut words: Vec<String> = line.split_whitespace().map(String::from).collect();
    if words.is_empty() {
        bail!("the #! line names no interpreter");
    }
    if words[0].ends_with("/env") {
        words.remove(0);
        if words.first().map(String::as_str) == Some("-S") {
            words.remove(0);
        }
        if words.is_empty() {
            bail!("#!/usr/bin/env names no interpreter");
        }
    }
    let base = words[0].rsplit('/').next().unwrap_or_default().to_string();
    let shell = matches!(
        base.as_str(),
        "sh" | "bash" | "dash" | "ash" | "ksh" | "zsh"
    );
    let python = base == "python" || base.starts_with("python3");
    if !shell && !python {
        bail!(
            "interpreter {:?} is not supported (a POSIX shell or python)",
            base
        );
    }
    Ok((words, shell))
}

/// POSIX shell run before the script: the per-pod SLURM_* variables, `srun` and `scontrol show hostnames`.
pub const PROLOGUE: &str = r#"export SLURM_PROCID="${RANK:-${JOB_COMPLETION_INDEX:-0}}"
export SLURM_NODEID="$SLURM_PROCID" SLURM_LOCALID=0
export SLURM_JOB_NODELIST="${MASTER_ADDR:-${HOSTNAME:-localhost}}"
export SLURM_NODELIST="$SLURM_JOB_NODELIST" SLURMD_NODENAME="${HOSTNAME:-localhost}"
export SLURM_SUBMIT_DIR="${SLURM_SUBMIT_DIR:-$PWD}" SLURM_CLUSTER_NAME=gryvia
srun() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --) shift; break ;;
      --*=*) shift ;;
      -n|-N|-c|-G|-t|-J|-p|-o|-e|-m|-w|-x|-C|-D|--ntasks|--nodes|--cpus-per-task|--gpus|--gpus-per-task|--gpus-per-node|--time|--job-name|--partition|--output|--error|--mpi|--gres|--mem|--mem-per-cpu|--ntasks-per-node|--cpu-bind|--distribution|--nodelist|--exclude|--chdir|--export|--container-image|--container-mounts) shift 2 ;;
      -*) shift ;;
      *) break ;;
    esac
  done
  "$@"
}
scontrol() {
  if [ "$1" = show ] && [ "$2" = hostnames ]; then echo "${MASTER_ADDR:-${HOSTNAME:-localhost}}"; return 0; fi
  echo "scontrol $*: not available in a Gryvia job" >&2
  return 1
}
"#;

fn env(name: &str, value: impl ToString) -> Value {
    json!({"name": name, "value": value.to_string()})
}

pub fn import(script: &str, opts: &Options) -> Result<Imported> {
    let mut warnings = Vec::new();
    let (interp, shell) = interpreter(script)?;
    let d = parse_directives(script, &mut warnings)?;

    let image = opts.image.clone().or(d.image.clone()).ok_or_else(|| {
        anyhow!("no container image: pass --image or add #SBATCH --container-image=<image>")
    })?;

    let (min_nodes, mut nodes) = d.nodes.unwrap_or((1, 1));
    let tasks_per_node = d.ntasks_per_node.unwrap_or(1).max(1);
    if d.nodes.is_none() {
        if let Some(n) = d.ntasks {
            nodes = n.div_ceil(tasks_per_node).max(1);
            warnings.push(format!(
                "--ntasks={} without --nodes: {} pod(s), one per node",
                n, nodes
            ));
        }
    }
    let min_nodes = if d.nodes.is_some() { min_nodes } else { nodes };

    let mut gpus = d.gpus_per_node.unwrap_or(0);
    if let Some(per_task) = d.gpus_per_task {
        gpus = per_task * tasks_per_node;
    }
    if let Some(total) = d.gpus_total {
        gpus = total.div_ceil(nodes);
        if total % nodes != 0 {
            warnings.push(format!(
                "--gpus={} over {} nodes: {} GPUs per pod",
                total, nodes, gpus
            ));
        }
    }
    if tasks_per_node > 1 && tasks_per_node != gpus.max(1) {
        warnings.push(format!(
            "--ntasks-per-node={}: each node runs one pod and srun runs its command once there; start {} processes yourself (torchrun --nproc-per-node)",
            tasks_per_node, tasks_per_node
        ));
    }

    let cpus = d
        .cpus_per_gpu
        .filter(|_| gpus > 0)
        .map(|c| c * gpus)
        .or(d.cpus_per_task.map(|c| c * tasks_per_node));
    let mem_mb = d
        .mem_mb
        .or(d.mem_per_cpu_mb.zip(cpus).map(|(m, c)| m * c as u64))
        .or(d
            .mem_per_gpu_mb
            .filter(|_| gpus > 0)
            .map(|m| m * gpus as u64));
    if d.mem_per_cpu_mb.is_some() && cpus.is_none() && d.mem_mb.is_none() {
        warnings.push("--mem-per-cpu without --cpus-per-task: no memory request is set".into());
    }

    let raw_name = d.name.clone().unwrap_or_else(|| opts.default_name.clone());
    let indexes: Vec<Option<u32>> = match &d.array {
        Some(ids) => ids.iter().copied().map(Some).collect(),
        None => vec![None],
    };
    let max_index_len = d
        .array
        .as_ref()
        .and_then(|a| a.last())
        .map(|i| i.to_string().len() + 1)
        .unwrap_or(0);
    let base = k8s_name(&raw_name, 63 - max_index_len);

    let mut spec = Map::new();
    spec.insert("type".into(), json!("training"));
    spec.insert("image".into(), json!(image));
    spec.insert("gpus".into(), json!(gpus));
    if let Some(t) = &d.gpu_type {
        spec.insert("gpuType".into(), json!(gpu_type_name(t)));
    }
    if let Some(secs) = d.time_secs {
        spec.insert("timeout".into(), json!(timeout_string(secs)));
    }
    if let Some(p) = &d.partition {
        spec.insert("queueName".into(), json!(p));
        warnings.push(format!(
            "--partition={}: set as queueName, the Kueue LocalQueue (used only with the Kueue integration)",
            p
        ));
    }
    if let Some(dir) = &d.chdir {
        spec.insert("workingDir".into(), json!(dir));
    }
    if nodes > 1 {
        let mut dist = json!({"enabled": true, "nodes": nodes});
        if gpus == 0 {
            dist["backend"] = json!("gloo");
        }
        if min_nodes < nodes {
            dist["elastic"] = json!({"minNodes": min_nodes});
            warnings.push(format!(
                "--nodes={}-{}: an elastic job; the script must cope with {} to {} workers (docs/elastic-training.md)",
                min_nodes, nodes, min_nodes, nodes
            ));
        }
        spec.insert("distributed".into(), dist);
    }
    let mut requests = Map::new();
    let mut limits = Map::new();
    if let Some(c) = cpus {
        requests.insert("cpu".into(), json!(c.to_string()));
    }
    if let Some(mb) = mem_mb {
        requests.insert("memory".into(), json!(quantity_mb(mb)));
        limits.insert("memory".into(), json!(quantity_mb(mb)));
    }
    if !requests.is_empty() {
        let mut res = json!({"requests": requests});
        if !limits.is_empty() {
            res["limits"] = Value::Object(limits);
        }
        spec.insert("resources".into(), res);
    }

    let mut static_env = vec![
        env("SLURM_JOB_NAME", &raw_name),
        env("SLURM_NNODES", nodes),
        env("SLURM_JOB_NUM_NODES", nodes),
        env("SLURM_NTASKS", nodes),
        env("SLURM_NPROCS", nodes),
        env("SLURM_NTASKS_PER_NODE", 1),
        env("SLURM_TASKS_PER_NODE", format!("1(x{})", nodes)),
    ];
    if let Some(p) = &d.partition {
        static_env.push(env("SLURM_JOB_PARTITION", p));
    }
    if let Some(c) = d.cpus_per_task {
        static_env.push(env("SLURM_CPUS_PER_TASK", c));
    }
    if let Some(c) = cpus {
        static_env.push(env("SLURM_CPUS_ON_NODE", c));
    }
    if gpus > 0 {
        static_env.push(env("SLURM_GPUS_ON_NODE", gpus));
    }
    if let Some(mb) = mem_mb {
        static_env.push(env("SLURM_MEM_PER_NODE", mb));
    }

    let (command, script_env) = if shell {
        let mut c = interp.clone();
        c.push("-c".into());
        c.push(format!("{}{}", PROLOGUE, script));
        (c, None)
    } else {
        let run = format!(
            "{}exec {} -c \"$GRYVIA_SBATCH_SCRIPT\"\n",
            PROLOGUE,
            interp.join(" ")
        );
        (
            vec!["/bin/sh".to_string(), "-c".into(), run],
            Some(script.to_string()),
        )
    };

    let mut jobs = Vec::new();
    for idx in &indexes {
        let name = match idx {
            Some(i) => format!("{}-{}", base, i),
            None => base.clone(),
        };
        let mut job_env = static_env.clone();
        job_env.push(env("SLURM_JOB_ID", &name));
        if let (Some(i), Some(ids)) = (idx, &d.array) {
            job_env.push(env("SLURM_ARRAY_JOB_ID", &base));
            job_env.push(env("SLURM_ARRAY_TASK_ID", i));
            job_env.push(env("SLURM_ARRAY_TASK_COUNT", ids.len()));
            job_env.push(env("SLURM_ARRAY_TASK_MIN", ids[0]));
            job_env.push(env("SLURM_ARRAY_TASK_MAX", ids[ids.len() - 1]));
        }
        if let Some(s) = &script_env {
            job_env.push(env("GRYVIA_SBATCH_SCRIPT", s));
        }
        let mut s = spec.clone();
        let mut command = command.clone();
        if shell {
            command.push(name.clone()); // $0 of the script
        }
        s.insert("command".into(), json!(command));
        s.insert("env".into(), Value::Array(job_env));
        let mut metadata = json!({
            "name": name,
            "labels": {"gryvia.io/imported-from": "sbatch"},
        });
        if let Some(ns) = &opts.namespace {
            metadata["namespace"] = json!(ns);
        }
        if d.array.is_some() {
            metadata["labels"]["gryvia.io/sbatch-array"] = json!(base);
        }
        jobs.push(json!({
            "apiVersion": "gryvia.io/v1alpha1",
            "kind": "GryviaAIJob",
            "metadata": metadata,
            "spec": Value::Object(s),
        }));
    }
    Ok(Imported { jobs, warnings })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn opts() -> Options {
        Options {
            image: Some("busybox:1.36".into()),
            default_name: "train".into(),
            namespace: None,
        }
    }

    fn one(script: &str) -> (Value, Vec<String>) {
        let mut out = import(script, &opts()).unwrap();
        assert_eq!(out.jobs.len(), 1);
        (out.jobs.remove(0), out.warnings)
    }

    fn env_of(job: &Value, name: &str) -> Option<String> {
        job["spec"]["env"]
            .as_array()
            .unwrap()
            .iter()
            .find(|e| e["name"] == name)
            .map(|e| e["value"].as_str().unwrap().to_string())
    }

    #[test]
    fn minimal_script_is_a_one_pod_cpu_job() {
        let (job, warnings) = one("#!/bin/bash\necho hi\n");
        assert!(warnings.is_empty(), "{:?}", warnings);
        assert_eq!(job["kind"], "GryviaAIJob");
        assert_eq!(job["metadata"]["name"], "train");
        assert_eq!(
            job["metadata"]["labels"]["gryvia.io/imported-from"],
            "sbatch"
        );
        assert_eq!(job["spec"]["type"], "training");
        assert_eq!(job["spec"]["gpus"], 0);
        assert!(job["spec"].get("distributed").is_none());
        let cmd = job["spec"]["command"].as_array().unwrap();
        assert_eq!(cmd[0], "/bin/bash");
        assert_eq!(cmd[1], "-c");
        let body = cmd[2].as_str().unwrap();
        assert!(body.starts_with(PROLOGUE));
        assert!(body.ends_with("#!/bin/bash\necho hi\n"));
        assert_eq!(cmd[3], "train");
        assert_eq!(env_of(&job, "SLURM_JOB_ID").as_deref(), Some("train"));
        assert_eq!(env_of(&job, "SLURM_NTASKS").as_deref(), Some("1"));
    }

    #[test]
    fn name_partition_time_and_chdir() {
        let (job, warnings) = one(
            "#!/bin/sh\n#SBATCH --job-name=My_Train.Run\n#SBATCH -p gpu\n#SBATCH -t 1-02:30:00\n#SBATCH -D /work\nrun\n",
        );
        assert_eq!(job["metadata"]["name"], "my-train-run");
        assert_eq!(
            env_of(&job, "SLURM_JOB_NAME").as_deref(),
            Some("My_Train.Run")
        );
        assert_eq!(job["spec"]["queueName"], "gpu");
        assert_eq!(env_of(&job, "SLURM_JOB_PARTITION").as_deref(), Some("gpu"));
        assert_eq!(job["spec"]["timeout"], "1d2h30m");
        assert_eq!(job["spec"]["workingDir"], "/work");
        assert_eq!(job["spec"]["command"][0], "/bin/sh");
        assert!(
            warnings.iter().any(|w| w.contains("Kueue")),
            "{:?}",
            warnings
        );
    }

    #[test]
    fn time_formats() {
        assert_eq!(parse_time("30").unwrap(), Some(1800));
        assert_eq!(parse_time("10:30").unwrap(), Some(630));
        assert_eq!(parse_time("02:00:00").unwrap(), Some(7200));
        assert_eq!(parse_time("2-12").unwrap(), Some(2 * 86400 + 12 * 3600));
        assert_eq!(parse_time("1-00:05").unwrap(), Some(86400 + 300));
        assert_eq!(parse_time("UNLIMITED").unwrap(), None);
        assert_eq!(parse_time("0").unwrap(), None);
        assert!(parse_time("1:2:3:4").is_err());
        assert!(parse_time("soon").is_err());
        assert_eq!(timeout_string(90 * 60), "1h30m");
        assert_eq!(timeout_string(45), "45s");
    }

    #[test]
    fn gres_and_gpu_options() {
        let (job, _) = one("#!/bin/bash\n#SBATCH --gres=gpu:a100:4\nx\n");
        assert_eq!(job["spec"]["gpus"], 4);
        assert_eq!(job["spec"]["gpuType"], "A100");
        assert_eq!(env_of(&job, "SLURM_GPUS_ON_NODE").as_deref(), Some("4"));
        let (job, _) = one("#!/bin/bash\n#SBATCH --gres=gpu:2\nx\n");
        assert_eq!(job["spec"]["gpus"], 2);
        assert!(job["spec"].get("gpuType").is_none());
        let (job, _) = one("#!/bin/bash\n#SBATCH --gres=gpu\nx\n");
        assert_eq!(job["spec"]["gpus"], 1);
        let (job, w) = one("#!/bin/bash\n#SBATCH --gres=gpu:tesla_v100:1,nvme:1\nx\n");
        assert_eq!(job["spec"]["gpuType"], "V100");
        assert!(w.iter().any(|w| w.contains("nvme:1")), "{:?}", w);
        let (job, _) = one("#!/bin/bash\n#SBATCH --gpus-per-node=h100:8\nx\n");
        assert_eq!(
            (
                job["spec"]["gpus"].as_u64(),
                job["spec"]["gpuType"].as_str()
            ),
            (Some(8), Some("H100"))
        );
        let (job, w) = one("#!/bin/bash\n#SBATCH -N 2\n#SBATCH -G 6\nx\n");
        assert_eq!(job["spec"]["gpus"], 3);
        assert!(w.is_empty(), "{:?}", w);
        let (job, w) = one("#!/bin/bash\n#SBATCH -N 2\n#SBATCH --gpus=3\nx\n");
        assert_eq!(job["spec"]["gpus"], 2);
        assert!(w.iter().any(|w| w.contains("2 GPUs per pod")), "{:?}", w);
        let (job, _) =
            one("#!/bin/bash\n#SBATCH --ntasks-per-node=4\n#SBATCH --gpus-per-task=1\nx\n");
        assert_eq!(job["spec"]["gpus"], 4);
    }

    #[test]
    fn nodes_make_a_distributed_job() {
        let (job, w) =
            one("#!/bin/bash\n#SBATCH --nodes=2\n#SBATCH --ntasks-per-node=1\nsrun hostname\n");
        assert_eq!(
            job["spec"]["distributed"],
            json!({"enabled": true, "nodes": 2, "backend": "gloo"})
        );
        assert_eq!(env_of(&job, "SLURM_NNODES").as_deref(), Some("2"));
        assert_eq!(env_of(&job, "SLURM_NTASKS").as_deref(), Some("2"));
        assert!(w.is_empty(), "{:?}", w);
        let (job, _) = one("#!/bin/bash\n#SBATCH -N2\n#SBATCH --gres=gpu:8\nx\n");
        assert_eq!(
            job["spec"]["distributed"],
            json!({"enabled": true, "nodes": 2})
        );
    }

    #[test]
    fn node_range_is_elastic() {
        let (job, w) = one("#!/bin/bash\n#SBATCH --nodes=2-4\nx\n");
        assert_eq!(job["spec"]["distributed"]["nodes"], 4);
        assert_eq!(job["spec"]["distributed"]["elastic"]["minNodes"], 2);
        assert!(w.iter().any(|w| w.contains("elastic")), "{:?}", w);
        assert!(parse_nodes("0").is_err());
        assert!(parse_nodes("4-2").is_err());
    }

    #[test]
    fn ntasks_without_nodes_is_one_pod_per_task() {
        let (job, w) = one("#!/bin/bash\n#SBATCH -n 3\nx\n");
        assert_eq!(job["spec"]["distributed"]["nodes"], 3);
        assert!(w.iter().any(|w| w.contains("--ntasks=3")), "{:?}", w);
        let (job, w) = one("#!/bin/bash\n#SBATCH --ntasks=4 --ntasks-per-node=2\nx\n");
        assert_eq!(job["spec"]["distributed"]["nodes"], 2);
        assert!(w.iter().any(|w| w.contains("torchrun")), "{:?}", w);
    }

    #[test]
    fn cpus_and_memory() {
        let (job, _) = one("#!/bin/bash\n#SBATCH --cpus-per-task=4\n#SBATCH --mem=16G\nx\n");
        assert_eq!(
            job["spec"]["resources"],
            json!({"requests": {"cpu": "4", "memory": "16Gi"}, "limits": {"memory": "16Gi"}})
        );
        assert_eq!(env_of(&job, "SLURM_CPUS_PER_TASK").as_deref(), Some("4"));
        assert_eq!(env_of(&job, "SLURM_MEM_PER_NODE").as_deref(), Some("16384"));
        let (job, _) = one("#!/bin/bash\n#SBATCH -c 2\n#SBATCH --mem-per-cpu=1500\nx\n");
        assert_eq!(job["spec"]["resources"]["requests"]["memory"], "3000Mi");
        let (job, _) = one("#!/bin/bash\n#SBATCH --gres=gpu:2\n#SBATCH --cpus-per-gpu=8\n#SBATCH --mem-per-gpu=40G\nx\n");
        assert_eq!(
            job["spec"]["resources"]["requests"],
            json!({"cpu": "16", "memory": "80Gi"})
        );
        let (job, w) = one("#!/bin/bash\n#SBATCH --mem=0\nx\n");
        assert!(job["spec"].get("resources").is_none());
        assert!(w.iter().any(|w| w.contains("--mem=0")), "{:?}", w);
        assert_eq!(parse_mem_mb("mem", "1T").unwrap(), 1024 * 1024);
        assert_eq!(parse_mem_mb("mem", "2048K").unwrap(), 2);
        assert!(parse_mem_mb("mem", "5X").is_err());
        assert_eq!(quantity_mb(1024 * 1024), "1Ti");
    }

    #[test]
    fn array_expands_to_one_job_per_index() {
        let out = import("#!/bin/bash\n#SBATCH -J sweep\n#SBATCH --array=0-6:3,10%2\necho $SLURM_ARRAY_TASK_ID\n", &opts()).unwrap();
        let names: Vec<_> = out
            .jobs
            .iter()
            .map(|j| j["metadata"]["name"].as_str().unwrap().to_string())
            .collect();
        assert_eq!(names, ["sweep-0", "sweep-3", "sweep-6", "sweep-10"]);
        let j = &out.jobs[1];
        assert_eq!(env_of(j, "SLURM_ARRAY_TASK_ID").as_deref(), Some("3"));
        assert_eq!(env_of(j, "SLURM_ARRAY_JOB_ID").as_deref(), Some("sweep"));
        assert_eq!(env_of(j, "SLURM_ARRAY_TASK_COUNT").as_deref(), Some("4"));
        assert_eq!(env_of(j, "SLURM_ARRAY_TASK_MIN").as_deref(), Some("0"));
        assert_eq!(env_of(j, "SLURM_ARRAY_TASK_MAX").as_deref(), Some("10"));
        assert_eq!(env_of(j, "SLURM_JOB_ID").as_deref(), Some("sweep-3"));
        assert_eq!(j["metadata"]["labels"]["gryvia.io/sbatch-array"], "sweep");
        assert_eq!(j["spec"]["command"][3], "sweep-3");
        assert!(
            out.warnings.iter().any(|w| w.contains("%2")),
            "{:?}",
            out.warnings
        );
        assert!(parse_array("0-1000").is_err());
        assert!(parse_array("5-1").is_err());
        assert_eq!(parse_array("3,1,3").unwrap().0, vec![1, 3]);
    }

    #[test]
    fn long_names_leave_room_for_the_array_index() {
        let long = "x".repeat(80);
        let out = import(
            &format!("#!/bin/bash\n#SBATCH -J {}\n#SBATCH -a 0-12\nx\n", long),
            &opts(),
        )
        .unwrap();
        for j in &out.jobs {
            assert!(j["metadata"]["name"].as_str().unwrap().len() <= 63);
        }
        assert_eq!(k8s_name("--", 63), "sbatch-job");
        assert_eq!(k8s_name("a__b", 63), "a-b");
    }

    #[test]
    fn ignored_and_unknown_directives_warn() {
        let (job, w) = one(
            "#!/bin/bash\n#SBATCH --output=slurm-%j.out\n#SBATCH -e err.log\n#SBATCH --dependency=afterok:1\n#SBATCH --exclusive\n#SBATCH --account=proj\n#SBATCH --frobnicate=3\nx\n",
        );
        assert_eq!(job["metadata"]["name"], "train");
        let all = w.join("\n");
        for needle in [
            "--output",
            "--error",
            "GryviaWorkflow",
            "--exclusive",
            "--account=proj",
            "--frobnicate=3",
        ] {
            assert!(all.contains(needle), "missing {:?} in {}", needle, all);
        }
    }

    #[test]
    fn directives_stop_at_the_first_command_and_handle_quotes_and_comments() {
        let (job, w) = one("#!/bin/bash\n\n# a comment\n#SBATCH --job-name \"two words\"  # trailing\necho start\n#SBATCH --nodes=4\n");
        assert_eq!(job["metadata"]["name"], "two-words");
        assert!(job["spec"].get("distributed").is_none());
        assert!(w.is_empty(), "{:?}", w);
        let (job, _) = one("#!/bin/bash\n#SBATCHX --nodes=4\nx\n");
        assert!(job["spec"].get("distributed").is_none());
    }

    #[test]
    fn image_from_pyxis_directive_or_flag() {
        let o = Options {
            image: None,
            default_name: "t".into(),
            namespace: Some("ml".into()),
        };
        let out = import(
            "#!/bin/bash\n#SBATCH --container-image=nvcr.io#nvidia/pytorch:24.01-py3\nx\n",
            &o,
        )
        .unwrap();
        assert_eq!(
            out.jobs[0]["spec"]["image"],
            "nvcr.io/nvidia/pytorch:24.01-py3"
        );
        assert_eq!(out.jobs[0]["metadata"]["namespace"], "ml");
        let (job, _) = one("#!/bin/bash\n#SBATCH --container-image=other:1\nx\n");
        assert_eq!(job["spec"]["image"], "busybox:1.36");
        let err = import("#!/bin/bash\nx\n", &Options::default())
            .unwrap_err()
            .to_string();
        assert!(err.contains("--image"), "{}", err);
    }

    #[test]
    fn interpreters() {
        assert!(import("echo no shebang\n", &opts())
            .unwrap_err()
            .to_string()
            .contains("#!"));
        assert!(import("#!/usr/bin/perl\nprint 1\n", &opts())
            .unwrap_err()
            .to_string()
            .contains("perl"));
        let (job, _) = one("#!/usr/bin/env bash\nx\n");
        assert_eq!(job["spec"]["command"][0], "bash");
        let (job, _) = one("#!/bin/bash -l\nx\n");
        assert_eq!(
            job["spec"]["command"].as_array().unwrap()[..3],
            [json!("/bin/bash"), json!("-l"), json!("-c")]
        );
        let (job, _) = one("#!/usr/bin/env python3\n#SBATCH -N 1\nprint('hi')\n");
        let cmd = job["spec"]["command"].as_array().unwrap();
        assert_eq!(cmd[0], "/bin/sh");
        assert!(cmd[2]
            .as_str()
            .unwrap()
            .ends_with("exec python3 -c \"$GRYVIA_SBATCH_SCRIPT\"\n"));
        assert_eq!(cmd.len(), 3);
        assert!(env_of(&job, "GRYVIA_SBATCH_SCRIPT")
            .unwrap()
            .contains("print('hi')"));
    }

    #[test]
    fn the_prologue_shims_slurm_under_sh() {
        let script = "#!/bin/sh\n#SBATCH -N 2\necho \"procid=$SLURM_PROCID nodeid=$SLURM_NODEID ntasks=$SLURM_NTASKS job=$SLURM_JOB_ID\"\nsrun --ntasks=1 -N 1 -l --mpi pmix echo srun-ran\nsrun -- echo after-dashes\nscontrol show hostnames \"$SLURM_JOB_NODELIST\" | head -n1\nscontrol show job || echo no-scontrol\n";
        let (job, _) = one(script);
        let cmd: Vec<String> = serde_json::from_value(job["spec"]["command"].clone()).unwrap();
        let mut c = std::process::Command::new(&cmd[0]);
        c.args(&cmd[1..]).env_clear().env("PATH", "/usr/bin:/bin");
        for e in job["spec"]["env"].as_array().unwrap() {
            c.env(e["name"].as_str().unwrap(), e["value"].as_str().unwrap());
        }
        let out = c
            .env("RANK", "1")
            .env("MASTER_ADDR", "train-0.train-headless")
            .output()
            .unwrap();
        let stdout = String::from_utf8(out.stdout).unwrap();
        assert!(
            out.status.success(),
            "{}{}",
            stdout,
            String::from_utf8_lossy(&out.stderr)
        );
        assert_eq!(
            stdout,
            "procid=1 nodeid=1 ntasks=2 job=train\nsrun-ran\nafter-dashes\ntrain-0.train-headless\nno-scontrol\n"
        );
        let out = std::process::Command::new(&cmd[0])
            .args(&cmd[1..])
            .env_clear()
            .env("PATH", "/usr/bin:/bin")
            .env("JOB_COMPLETION_INDEX", "0")
            .output()
            .unwrap();
        assert!(String::from_utf8(out.stdout)
            .unwrap()
            .starts_with("procid=0 "));
    }

    #[test]
    fn bad_values_are_errors() {
        for s in [
            "#SBATCH --nodes=two",
            "#SBATCH --time=later",
            "#SBATCH --gres=gpu:a:b:c",
            "#SBATCH --nodes",
            "#SBATCH stray",
        ] {
            assert!(
                import(&format!("#!/bin/bash\n{}\nx\n", s), &opts()).is_err(),
                "{}",
                s
            );
        }
    }
}
