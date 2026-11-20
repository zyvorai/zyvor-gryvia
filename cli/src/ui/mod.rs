// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//! The terminal look shared by every command: when to use color, status markers, section headers and tables.
//!
//! Keep all styling here so `--no-color`, `NO_COLOR` and piping behave the same everywhere.

pub mod help;

use std::io::IsTerminal;

/// Decide whether to emit ANSI colors. Pure, so the rules are testable.
///
/// Order: `--no-color` wins, then a non-empty `NO_COLOR` (https://no-color.org), then `CLICOLOR_FORCE`
/// (anything but empty or `0`), otherwise color only when stdout is a terminal.
pub fn decide_color(
    no_color_flag: bool,
    no_color_env: Option<&str>,
    clicolor_force: Option<&str>,
    stdout_is_tty: bool,
) -> bool {
    if no_color_flag {
        return false;
    }
    if no_color_env.is_some_and(|v| !v.is_empty()) {
        return false;
    }
    if clicolor_force.is_some_and(|v| !v.is_empty() && v != "0") {
        return true;
    }
    stdout_is_tty
}

/// True when the command line asks for no color (`--no-color`), before clap has parsed anything.
pub fn args_request_no_color(args: &[String]) -> bool {
    args.iter().any(|a| a == "--no-color")
}

/// Apply the color decision to the `colored` crate. Call once at startup.
pub fn init(no_color_flag: bool) {
    let on = decide_color(
        no_color_flag,
        std::env::var("NO_COLOR").ok().as_deref(),
        std::env::var("CLICOLOR_FORCE").ok().as_deref(),
        std::io::stdout().is_terminal(),
    );
    colored::control::set_override(on);
}

/// Wrap `text` in ANSI `codes` (for example `"1;31"`) when `color` is true. Takes the decision as an argument, so
/// pure renderers stay independent of global state and can be tested in parallel.
pub fn ansi(text: &str, codes: &str, color: bool) -> String {
    if color {
        format!("\x1b[{codes}m{text}\x1b[0m")
    } else {
        text.to_string()
    }
}

/// True when output should currently be colored (after [`init`]).
pub fn color_enabled() -> bool {
    colored::control::SHOULD_COLORIZE.should_colorize()
}

/// Health of a component, as shown in status output.
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Marker {
    Ok,
    Warn,
    Error,
    Disabled,
    Unknown,
}

impl Marker {
    /// The word printed after a component name.
    pub fn label(self) -> &'static str {
        match self {
            Marker::Ok => "OK",
            Marker::Warn => "Warning",
            Marker::Error => "Error",
            Marker::Disabled => "disabled",
            Marker::Unknown => "unknown",
        }
    }

    pub fn glyph(self) -> &'static str {
        match self {
            Marker::Ok => "✓",
            Marker::Warn => "!",
            Marker::Error => "✗",
            Marker::Disabled => "-",
            Marker::Unknown => "?",
        }
    }

    fn codes(self) -> &'static str {
        match self {
            Marker::Ok => "32",
            Marker::Warn => "33",
            Marker::Error => "1;31",
            Marker::Disabled | Marker::Unknown => "2",
        }
    }

    /// `text` in this marker's color when `color` is true.
    pub fn paint_with(self, text: &str, color: bool) -> String {
        ansi(text, self.codes(), color)
    }

    /// `text` painted in this marker's color, following the terminal decision made at startup.
    pub fn paint(self, text: &str) -> String {
        self.paint_with(text, color_enabled())
    }

    /// The glyph, painted.
    pub fn mark(self) -> String {
        self.paint(self.glyph())
    }

    /// The marker for a resource phase (`Running`, `Pending`, `Failed`, ...), case-insensitive.
    pub fn from_phase(phase: &str) -> Marker {
        match phase.trim().to_ascii_lowercase().as_str() {
            "running" | "active" | "ready" | "healthy" | "succeeded" | "completed" | "bound" => {
                Marker::Ok
            }
            "pending" | "queued" | "scheduling" | "initializing" | "provisioning"
            | "terminating" | "deploying" => Marker::Warn,
            "failed" | "error" | "unhealthy" | "quotaexceeded" | "budgetexceeded" => Marker::Error,
            _ => Marker::Unknown,
        }
    }

    /// The worse of two markers (Error > Warn > Unknown > Ok > Disabled), for rolling components up.
    pub fn worst(self, other: Marker) -> Marker {
        fn rank(m: Marker) -> u8 {
            match m {
                Marker::Error => 4,
                Marker::Warn => 3,
                Marker::Unknown => 2,
                Marker::Ok => 1,
                Marker::Disabled => 0,
            }
        }
        if rank(other) > rank(self) {
            other
        } else {
            self
        }
    }
}

/// A command's top banner: `━━━ Title ━━━`.
pub fn header(title: &str, color: bool) -> String {
    ansi(&format!("━━━ {title} ━━━"), "1;36", color)
}

/// An aligned `key  value` line, flush left. `width` is the key column width.
pub fn kv(key: &str, value: &str, width: usize, color: bool) -> String {
    format!("{}{}", ansi(&format!("{key:<width$}"), "1", color), value)
}

/// A section heading inside a command's output.
pub fn section(title: &str, color: bool) -> String {
    ansi(title, "1;4", color)
}

/// One table cell: its text and, optionally, the marker whose color it should carry.
pub type Cell2 = (String, Option<Marker>);

/// Aligned columns without borders (like `kubectl get`). Widths come from the plain text, and color is
/// applied after padding, so ANSI codes never disturb alignment.
pub fn grid(headers: &[&str], rows: &[Vec<Cell2>], color: bool) -> Vec<String> {
    let mut widths: Vec<usize> = headers.iter().map(|h| h.chars().count()).collect();
    for row in rows {
        for (i, (text, _)) in row.iter().enumerate() {
            if i < widths.len() {
                widths[i] = widths[i].max(text.chars().count());
            }
        }
    }
    // A trailing run of empty cells adds nothing; drop it so lines never end in separators or codes.
    let rows: Vec<Vec<Cell2>> = rows
        .iter()
        .map(|row| {
            let keep = row
                .iter()
                .rposition(|(text, _)| !text.is_empty())
                .map_or(0, |i| i + 1);
            row[..keep].to_vec()
        })
        .collect();
    let mut lines = Vec::new();
    let head: Vec<String> = headers
        .iter()
        .enumerate()
        .map(|(i, h)| ansi(&pad_unless_last(h, widths[i], i, headers.len()), "1", color))
        .collect();
    lines.push(head.join("  ").trim_end().to_string());
    for row in &rows {
        let cells: Vec<String> = row
            .iter()
            .enumerate()
            .map(|(i, (text, marker))| {
                // Pad every cell that is followed by another one on this line.
                let padded = if i + 1 < row.len() {
                    pad(text, widths.get(i).copied().unwrap_or(0))
                } else {
                    text.to_string()
                };
                match marker {
                    Some(m) => m.paint_with(&padded, color),
                    None => padded,
                }
            })
            .collect();
        lines.push(cells.join("  ").trim_end().to_string());
    }
    lines
}

/// Pad to `width`, except in the last column, so lines never end in spaces (with or without color).
fn pad_unless_last(text: &str, width: usize, index: usize, columns: usize) -> String {
    if index + 1 >= columns {
        text.to_string()
    } else {
        pad(text, width)
    }
}

fn pad(text: &str, width: usize) -> String {
    let len = text.chars().count();
    if len >= width {
        text.to_string()
    } else {
        format!("{text}{}", " ".repeat(width - len))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn color_rules() {
        // flag beats everything
        assert!(!decide_color(true, None, Some("1"), true));
        // NO_COLOR (non-empty) beats CLICOLOR_FORCE and the terminal
        assert!(!decide_color(false, Some("1"), Some("1"), true));
        // an empty NO_COLOR is ignored
        assert!(decide_color(false, Some(""), None, true));
        // CLICOLOR_FORCE turns color on when piped, unless it is 0 or empty
        assert!(decide_color(false, None, Some("1"), false));
        assert!(!decide_color(false, None, Some("0"), false));
        assert!(!decide_color(false, None, Some(""), false));
        // otherwise follow the terminal
        assert!(decide_color(false, None, None, true));
        assert!(!decide_color(false, None, None, false));
    }

    #[test]
    fn no_color_flag_is_seen_before_parsing() {
        let args = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        assert!(args_request_no_color(&args(&[
            "gryvia",
            "--no-color",
            "status"
        ])));
        assert!(!args_request_no_color(&args(&["gryvia", "status"])));
    }

    #[test]
    fn markers_roll_up_to_the_worst() {
        assert_eq!(Marker::Ok.worst(Marker::Warn), Marker::Warn);
        assert_eq!(Marker::Warn.worst(Marker::Error), Marker::Error);
        assert_eq!(Marker::Error.worst(Marker::Ok), Marker::Error);
        assert_eq!(Marker::Disabled.worst(Marker::Ok), Marker::Ok);
        assert_eq!(Marker::Ok.worst(Marker::Disabled), Marker::Ok);
    }

    #[test]
    fn marker_words_and_glyphs() {
        assert_eq!(Marker::Ok.paint_with(Marker::Ok.label(), false), "OK");
        assert_eq!(Marker::Error.paint_with(Marker::Error.glyph(), false), "✗");
        assert_eq!(Marker::Disabled.label(), "disabled");
        assert!(Marker::Error.paint_with("x", true).contains("\x1b[1;31m"));
        assert_eq!(ansi("x", "1", false), "x");
    }

    #[test]
    fn grid_aligns_on_plain_text_even_with_color() {
        let rows = vec![
            vec![
                ("node-a".to_string(), None),
                ("OK".to_string(), Some(Marker::Ok)),
            ],
            vec![
                ("n".to_string(), None),
                ("Error".to_string(), Some(Marker::Error)),
            ],
        ];
        let plain = grid(&["NODE", "STATE"], &rows, false);
        assert_eq!(plain, vec!["NODE    STATE", "node-a  OK", "n       Error"]);
        // The colored form pads identically, the codes only wrap the padded text.
        let strip = |s: &str| {
            let mut out = String::new();
            let mut skip = false;
            for c in s.chars() {
                if c == '\x1b' {
                    skip = true;
                } else if skip && c == 'm' {
                    skip = false;
                } else if !skip {
                    out.push(c);
                }
            }
            out
        };
        let colored: Vec<String> = grid(&["NODE", "STATE"], &rows, true)
            .iter()
            .map(|l| strip(l))
            .collect();
        assert_eq!(colored, plain);
    }

    #[test]
    fn phases_map_to_markers() {
        for ok in ["Running", "ready", "Healthy", "Completed", "Succeeded"] {
            assert_eq!(Marker::from_phase(ok), Marker::Ok, "{ok}");
        }
        for warn in ["Pending", "queued", "Scheduling", "Initializing"] {
            assert_eq!(Marker::from_phase(warn), Marker::Warn, "{warn}");
        }
        for bad in ["Failed", "QuotaExceeded", "error"] {
            assert_eq!(Marker::from_phase(bad), Marker::Error, "{bad}");
        }
        assert_eq!(Marker::from_phase(""), Marker::Unknown);
        assert_eq!(Marker::from_phase("Weird"), Marker::Unknown);
    }

    #[test]
    fn headers_and_key_value_lines() {
        assert_eq!(header("Cluster", false), "━━━ Cluster ━━━");
        assert_eq!(kv("Nodes:", "3", 10, false), "Nodes:    3");
        assert!(kv("Nodes:", "3", 10, true).contains("\x1b[1m"));
    }

    #[test]
    fn sections_are_plain_without_color() {
        assert_eq!(section("Nodes", false), "Nodes");
    }
}
