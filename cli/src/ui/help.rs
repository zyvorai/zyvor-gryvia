//! Root `--help`: commands in named groups, colored, like `cilium --help`.
//!
//! clap has no native command groups, so the root help is rendered here. Names and descriptions come from
//! the clap `Command` itself (one source of truth); only the grouping lives in [`GROUPS`]. Per-command help
//! stays clap's own, styled through `Styles`.

use clap::Command;

/// Command groups in display order. A unit test fails if a subcommand is missing from (or repeated in) here.
pub const GROUPS: &[(&str, &[&str])] = &[
    (
        "Workloads",
        &[
            "submit",
            "workspace",
            "create",
            "validate",
            "list",
            "get",
            "status",
            "logs",
            "cancel",
            "delete",
        ],
    ),
    (
        "Cluster",
        &["cluster", "queue", "capacity", "quota", "cost"],
    ),
    (
        "Tenants & Billing",
        &[
            "tenant",
            "catalog",
            "usage",
            "invoice",
            "reservation",
            "budget",
        ],
    ),
    (
        "ML",
        &["models", "datasets", "llm", "rag", "agents", "job-hooks"],
    ),
    ("Operations", &["health", "maintenance"]),
    ("Observe", &["network", "security", "gpu"]),
    ("Utilities", &["completion", "version"]),
];

fn paint(text: &str, codes: &str, color: bool) -> String {
    if color {
        format!("\x1b[{codes}m{text}\x1b[0m")
    } else {
        text.to_string()
    }
}

fn heading(text: &str, color: bool) -> String {
    paint(text, "1;36", color)
}

fn literal(text: &str, color: bool) -> String {
    paint(text, "1;32", color)
}

fn dim(text: &str, color: bool) -> String {
    paint(text, "2", color)
}

/// First line of a command's `about`.
fn about(cmd: &Command) -> String {
    cmd.get_about()
        .map(|s| s.to_string())
        .unwrap_or_default()
        .lines()
        .next()
        .unwrap_or("")
        .to_string()
}

/// Left column of an option: `-n, --namespace <NAMESPACE>`.
fn option_label(arg: &clap::Arg) -> String {
    let mut label = String::new();
    match (arg.get_short(), arg.get_long()) {
        (Some(s), Some(l)) => label.push_str(&format!("-{s}, --{l}")),
        (Some(s), None) => label.push_str(&format!("-{s}")),
        (None, Some(l)) => label.push_str(&format!("    --{l}")),
        (None, None) => {}
    }
    if arg.get_action().takes_values() {
        label.push_str(&format!(" <{}>", arg.get_id().as_str().to_uppercase()));
    }
    label
}

/// The grouped root help text. `color` selects ANSI styling (callers pass the terminal decision).
pub fn render_root_help(cmd: &Command, color: bool) -> String {
    let mut out = String::new();
    if let Some(about) = cmd.get_about() {
        out.push_str(&format!("{about}\n\n"));
    }
    out.push_str(&format!(
        "{} {} {}\n",
        heading("Usage:", color),
        literal(cmd.get_name(), color),
        dim("[OPTIONS] <COMMAND>", color)
    ));

    let subcommands: Vec<&Command> = cmd
        .get_subcommands()
        .filter(|c| !c.is_hide_set() && c.get_name() != "help")
        .collect();
    let width = subcommands
        .iter()
        .map(|c| c.get_name().len())
        .max()
        .unwrap_or(0)
        + 2;

    let mut placed: Vec<&str> = Vec::new();
    for (group, names) in GROUPS {
        out.push_str(&format!("\n{}\n", heading(&format!("{group}:"), color)));
        for name in *names {
            if let Some(sub) = subcommands.iter().find(|c| c.get_name() == *name) {
                out.push_str(&format!(
                    "  {}{}\n",
                    literal(&format!("{name:<width$}"), color),
                    about(sub)
                ));
                placed.push(name);
            }
        }
    }
    let others: Vec<&&Command> = subcommands
        .iter()
        .filter(|c| !placed.contains(&c.get_name()))
        .collect();
    if !others.is_empty() {
        out.push_str(&format!("\n{}\n", heading("Other Commands:", color)));
        for sub in others {
            out.push_str(&format!(
                "  {}{}\n",
                literal(&format!("{:<width$}", sub.get_name()), color),
                about(sub)
            ));
        }
    }

    // Options: the command's own arguments, then help and version.
    let mut options: Vec<(String, String)> = cmd
        .get_arguments()
        .filter(|a| a.get_id() != "help" && a.get_id() != "version")
        .filter(|a| a.get_short().is_some() || a.get_long().is_some())
        .map(|a| {
            (
                option_label(a),
                a.get_help().map(|h| h.to_string()).unwrap_or_default(),
            )
        })
        .collect();
    options.push(("-h, --help".to_string(), "Print help".to_string()));
    options.push(("-V, --version".to_string(), "Print version".to_string()));
    let opt_width = options.iter().map(|(l, _)| l.len()).max().unwrap_or(0) + 2;
    out.push_str(&format!("\n{}\n", heading("Options:", color)));
    for (label, help) in options {
        out.push_str(&format!(
            "  {}{}\n",
            literal(&format!("{label:<opt_width$}"), color),
            help
        ));
    }

    out.push_str(&format!(
        "\n{}\n",
        dim(
            &format!(
                "Use \"{} <command> --help\" for more information about a command.",
                cmd.get_name()
            ),
            color
        )
    ));
    out
}

/// True when the arguments ask for the root help: no command, or a single `-h`, `--help` or `help`.
/// `--no-color` does not count as a command, so `gryvia --no-color` still shows the help.
pub fn wants_root_help(args: &[String]) -> bool {
    let rest: Vec<&str> = args
        .iter()
        .skip(1)
        .map(String::as_str)
        .filter(|a| *a != "--no-color")
        .collect();
    match rest.as_slice() {
        [] => true,
        [one] => matches!(*one, "-h" | "--help" | "help"),
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use clap::CommandFactory;

    fn cmd() -> Command {
        crate::Cli::command()
    }

    #[test]
    fn every_subcommand_is_in_exactly_one_group() {
        let cmd = cmd();
        let names: Vec<String> = cmd
            .get_subcommands()
            .filter(|c| !c.is_hide_set() && c.get_name() != "help")
            .map(|c| c.get_name().to_string())
            .collect();
        for name in &names {
            let count = GROUPS
                .iter()
                .filter(|(_, members)| members.contains(&name.as_str()))
                .count();
            assert_eq!(
                count, 1,
                "subcommand `{name}` must be in exactly one help group"
            );
        }
        for (group, members) in GROUPS {
            for member in *members {
                assert!(
                    names.iter().any(|n| n == member),
                    "group `{group}` lists `{member}`, which is not a subcommand"
                );
            }
        }
    }

    #[test]
    fn plain_help_has_the_groups_and_no_escape_codes() {
        let text = render_root_help(&cmd(), false);
        for group in [
            "Workloads:",
            "Cluster:",
            "Operations:",
            "Observe:",
            "Utilities:",
            "Options:",
        ] {
            assert!(text.contains(group), "missing {group}");
        }
        assert!(text.contains("submit"));
        assert!(text.contains("--no-color"));
        assert!(!text.contains('\x1b'));
    }

    #[test]
    fn colored_help_has_escape_codes() {
        assert!(render_root_help(&cmd(), true).contains("\x1b[1;36m"));
    }

    #[test]
    fn help_is_grouped_in_the_documented_order() {
        let text = render_root_help(&cmd(), false);
        let positions: Vec<usize> = [
            "Workloads:",
            "Cluster:",
            "Operations:",
            "Observe:",
            "Utilities:",
        ]
        .iter()
        .map(|g| text.find(g).unwrap())
        .collect();
        assert!(positions.windows(2).all(|w| w[0] < w[1]));
    }

    #[test]
    fn root_help_is_requested_by_the_right_arguments() {
        let a = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        assert!(wants_root_help(&a(&["gryvia"])));
        assert!(wants_root_help(&a(&["gryvia", "--help"])));
        assert!(wants_root_help(&a(&["gryvia", "-h"])));
        assert!(wants_root_help(&a(&["gryvia", "help"])));
        assert!(!wants_root_help(&a(&["gryvia", "help", "submit"])));
        assert!(!wants_root_help(&a(&["gryvia", "list", "jobs"])));
        assert!(wants_root_help(&a(&["gryvia", "--no-color"])));
        assert!(wants_root_help(&a(&["gryvia", "--no-color", "--help"])));
        assert!(!wants_root_help(&a(&[
            "gryvia",
            "--no-color",
            "list",
            "jobs"
        ])));
    }
}
