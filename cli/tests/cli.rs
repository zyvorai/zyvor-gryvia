// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! End-to-end checks of the built binary. None of these need a cluster: they run with no kubeconfig.

use assert_cmd::Command;
use predicates::prelude::*;

fn gryvia() -> Command {
    let mut cmd = Command::cargo_bin("gryvia").unwrap();
    cmd.env("KUBECONFIG", "/nonexistent/kubeconfig")
        .env("HOME", "/nonexistent")
        .env_remove("NO_COLOR")
        .env_remove("CLICOLOR_FORCE");
    cmd
}

#[test]
fn root_help_is_grouped_and_needs_no_cluster() {
    gryvia()
        .arg("--help")
        .assert()
        .success()
        .stdout(predicate::str::contains("Workloads:"))
        .stdout(predicate::str::contains("Cluster:"))
        .stdout(predicate::str::contains("Operations:"))
        .stdout(predicate::str::contains("Observe:"))
        .stdout(predicate::str::contains("Utilities:"))
        .stdout(predicate::str::contains("--no-color"));
}

#[test]
fn no_arguments_shows_the_same_help() {
    gryvia()
        .assert()
        .success()
        .stdout(predicate::str::contains("Workloads:"));
}

#[test]
fn piped_help_has_no_escape_codes() {
    gryvia()
        .arg("--help")
        .assert()
        .success()
        .stdout(predicate::str::contains('\x1b').not());
}

#[test]
fn forced_color_adds_escape_codes_and_no_color_wins() {
    gryvia()
        .env("CLICOLOR_FORCE", "1")
        .arg("--help")
        .assert()
        .success()
        .stdout(predicate::str::contains('\x1b'));
    gryvia()
        .env("CLICOLOR_FORCE", "1")
        .arg("--no-color")
        .arg("--help")
        .assert()
        .success()
        .stdout(predicate::str::contains('\x1b').not());
    gryvia()
        .env("CLICOLOR_FORCE", "1")
        .env("NO_COLOR", "1")
        .arg("--help")
        .assert()
        .success()
        .stdout(predicate::str::contains('\x1b').not());
}

#[test]
fn subcommand_help_has_examples() {
    gryvia()
        .args(["list", "--help"])
        .assert()
        .success()
        .stdout(predicate::str::contains("Examples:"))
        .stdout(predicate::str::contains("gryvia list jobs"));
}

#[test]
fn completions_generate_for_each_shell() {
    for shell in ["bash", "zsh", "fish", "powershell"] {
        gryvia()
            .args(["completion", shell])
            .assert()
            .success()
            .stdout(predicate::str::contains("gryvia"));
    }
}

#[test]
fn version_client_needs_no_cluster() {
    gryvia()
        .args(["version", "--client"])
        .assert()
        .success()
        .stdout(predicate::str::contains("gryvia "));
}

#[test]
fn unknown_flags_and_bad_formats_exit_2() {
    gryvia().arg("--bogus").assert().code(2);
    gryvia()
        .args(["list", "jobs", "-o", "xml"])
        .assert()
        .code(2);
}

#[test]
fn validate_works_without_a_cluster() {
    let path = std::env::temp_dir().join(format!("gryvia-cli-test-{}.yaml", std::process::id()));
    std::fs::write(
        &path,
        "apiVersion: gryvia.io/v1alpha1\nkind: GryviaAIJob\nmetadata:\n  name: t\nspec:\n  type: training\n  gpus: 1\n  image: busybox\n",
    )
    .unwrap();
    gryvia()
        .args(["validate", path.to_str().unwrap()])
        .assert()
        .success();
    let _ = std::fs::remove_file(path);
}

/// A one-thread mock gateway: answers each request with the body of the first route whose path matches
/// (401 when the bearer key is not `test-key`), and stops after `requests` requests.
fn mock_gateway(routes: Vec<(&'static str, String)>, requests: usize) -> String {
    use std::io::{Read, Write};
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    std::thread::spawn(move || {
        for _ in 0..requests {
            let Ok((mut sock, _)) = listener.accept() else {
                return;
            };
            let mut buf = [0u8; 8192];
            let n = sock.read(&mut buf).unwrap_or(0);
            let text = String::from_utf8_lossy(&buf[..n]).to_string();
            let path = text
                .lines()
                .next()
                .and_then(|l| l.split_whitespace().nth(1))
                .unwrap_or("/")
                .split('?')
                .next()
                .unwrap()
                .to_string();
            let authed = text
                .to_ascii_lowercase()
                .contains("authorization: bearer test-key");
            let (status, body) = match routes.iter().find(|(p, _)| *p == path) {
                _ if !authed => (401, "{\"detail\":\"Invalid token\"}".to_string()),
                Some((_, b)) => (200, b.clone()),
                None => (404, "{\"detail\":\"Not Found\"}".to_string()),
            };
            let resp = format!(
                "HTTP/1.1 {status} X\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                body.len()
            );
            let _ = sock.write_all(resp.as_bytes());
        }
    });
    url
}

#[test]
fn network_flows_and_graph_use_the_gateway_without_a_cluster() {
    let flows = r#"{"source":"netra","items":[{"metadata":{"name":"netra-0"},"spec":{"timestamp":"2026-01-01T00:00:00Z",
        "source":"shop/web-1","destination":"api","protocol":"TCP","port":8080,"bytes":"42","latency":"1.5ms","verdict":"FORWARDED"}}]}"#;
    let graph = r#"{"nodes":[{"id":"web"},{"id":"api"}],"edges":[{"source":"web","target":"api","protocol":"tcp","latency":"4ms","verdict":""}]}"#;
    let url = mock_gateway(
        vec![
            ("/api/network/flows", flows.to_string()),
            ("/api/network/graph", graph.to_string()),
        ],
        3,
    );
    gryvia()
        .env("GRYVIA_API_KEY", "test-key")
        .args(["network", "flows", "--gateway", &url])
        .assert()
        .success()
        .stdout(predicate::str::contains("gateway (Netra flow history)"))
        .stdout(predicate::str::contains("shop/web-1"))
        .stdout(predicate::str::contains("FORWARDED"));
    gryvia()
        .env("GRYVIA_API_KEY", "test-key")
        .env("GRYVIA_GATEWAY_URL", &url)
        .args(["network", "graph", "--format", "json"])
        .assert()
        .success()
        .stdout(predicate::str::contains("\"target\": \"api\""));
    // wrong key: a concise error naming the HTTP status and the fix, not a stack of causes
    gryvia()
        .env("GRYVIA_API_KEY", "wrong")
        .args(["network", "flows", "--gateway", &url])
        .assert()
        .failure()
        .stderr(predicate::str::contains("HTTP 401"))
        .stderr(predicate::str::contains("GRYVIA_API_KEY"));
}

#[test]
fn gpu_memory_uses_the_gateway_and_says_what_it_needs_without_one() {
    let body = r#"{"h2dBytes":2048,"h2dCount":2,"d2hBytes":0,"d2hCount":0,"d2dBytes":0,"d2dCount":0,"collectors":{"reachable":1,"total":1}}"#;
    let url = mock_gateway(vec![("/api/gpu/memory", body.to_string())], 1);
    gryvia()
        .env("GRYVIA_API_KEY", "test-key")
        .args(["gpu", "memory", "--gateway", &url])
        .assert()
        .success()
        .stdout(predicate::str::contains("2.0 KiB"))
        .stdout(predicate::str::contains("1 of 1 collectors answered"));
    // no gateway: no placeholder numbers, only what is needed (and no cluster required)
    gryvia()
        .env_remove("GRYVIA_GATEWAY_URL")
        .args(["gpu", "memory"])
        .assert()
        .success()
        .stdout(predicate::str::contains("GRYVIA_GATEWAY_URL"));
}
