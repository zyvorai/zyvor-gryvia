// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

use anyhow::{Context, Result};
use futures::{AsyncBufReadExt, StreamExt};
use k8s_openapi::api::core::v1::Pod;
use kube::api::{Api, ListParams, LogParams};
use kube::api::{ApiResource, GroupVersionKind};
use kube::core::DynamicObject;

use crate::client::GryviaClient;
use crate::display;

pub async fn execute(
    client: &GryviaClient,
    job: &str,
    follow: bool,
    tail: usize,
    replica: Option<usize>,
) -> Result<()> {
    // Validate job name to prevent label selector injection
    if job.contains('=') || job.contains(',') || job.contains('!') {
        anyhow::bail!(
            "Invalid job name '{}': contains reserved label selector characters",
            job
        );
    }

    let pods_api: Api<Pod> = Api::namespaced(client.kube_client.clone(), client.namespace());

    let label_selector = format!("gryvia.io/job={}", job);
    let lp = ListParams::default().labels(&label_selector);

    let pods = pods_api
        .list(&lp)
        .await
        .context("Failed to list pods for job")?;

    if pods.items.is_empty() {
        let ar = ApiResource::from_gvk(&GroupVersionKind::gvk(
            "gryvia.io",
            "v1alpha1",
            "GryviaAIJob",
        ));
        let jobs_api: Api<DynamicObject> =
            Api::namespaced_with(client.kube_client.clone(), client.namespace(), &ar);
        let _ = jobs_api
            .get(job)
            .await
            .with_context(|| format!("Job '{}' not found", job))?;

        display::print_warning(&format!(
            "No pods found for job '{}' - job may still be scheduling",
            job
        ));
        return Ok(());
    }

    let target_pod = select_pod(&pods.items, replica).with_context(|| match replica {
        Some(idx) => format!("Replica {} not found for job '{}'", idx, job),
        None => format!("No pod found for job '{}'", job),
    })?;

    let pod_name = target_pod.metadata.name.as_deref().unwrap_or("unknown");
    display::print_info(&format!("Streaming logs from pod: {}", pod_name));

    // Detect container name from the pod spec, defaulting to "trainer"
    let container_name = target_pod
        .spec
        .as_ref()
        .and_then(|s| s.containers.first())
        .map(|c| c.name.clone())
        .unwrap_or_else(|| "trainer".to_string());

    let log_params = LogParams {
        follow,
        tail_lines: Some(tail as i64),
        container: Some(container_name),
        ..Default::default()
    };

    if follow {
        let stream = pods_api
            .log_stream(pod_name, &log_params)
            .await
            .context("Failed to start log stream")?;

        let mut lines = stream.lines();
        while let Some(line) = lines.next().await {
            match line {
                Ok(l) => println!("{}", l),
                Err(e) => {
                    display::print_error(&format!("Log stream error: {}", e));
                    break;
                }
            }
        }
    } else {
        let logs = pods_api
            .logs(pod_name, &log_params)
            .await
            .context("Failed to get pod logs")?;
        print!("{}", logs);
    }

    Ok(())
}

/// Completion index / ordinal of a pod of a job, whichever workload backs it:
/// - Indexed batch Job pods (`<job>-<index>-<random>`): annotation/label
///   `batch.kubernetes.io/job-completion-index`;
/// - StatefulSet pods (`<job>-training-<ordinal>`): label `apps.kubernetes.io/pod-index`
///   (Kubernetes >= 1.28), else the numeric suffix of the pod name.
fn pod_index(pod: &Pod) -> Option<usize> {
    const JOB_INDEX: &str = "batch.kubernetes.io/job-completion-index";
    const STS_INDEX: &str = "apps.kubernetes.io/pod-index";
    let meta = &pod.metadata;
    let from_map = |m: &Option<std::collections::BTreeMap<String, String>>, key: &str| {
        m.as_ref()
            .and_then(|m| m.get(key))
            .and_then(|v| v.parse::<usize>().ok())
    };
    if let Some(i) =
        from_map(&meta.annotations, JOB_INDEX).or_else(|| from_map(&meta.labels, JOB_INDEX))
    {
        return Some(i);
    }
    if let Some(i) = from_map(&meta.labels, STS_INDEX) {
        return Some(i);
    }
    let owned_by_sts = meta
        .owner_references
        .as_ref()
        .map(|o| o.iter().any(|r| r.kind == "StatefulSet"))
        .unwrap_or(false);
    if owned_by_sts {
        return meta
            .name
            .as_deref()
            .and_then(|n| n.rsplit('-').next())
            .and_then(|s| s.parse().ok());
    }
    None
}

/// Pick the pod to read: the pod with the requested index, or (no index) the lowest index.
/// A failed Job pod is replaced by a new pod with the same index, so several pods can share
/// one index: prefer one that has not terminated, then the newest.
fn select_pod(pods: &[Pod], index: Option<usize>) -> Option<&Pod> {
    let terminated = |p: &Pod| {
        matches!(
            p.status.as_ref().and_then(|s| s.phase.as_deref()),
            Some("Failed") | Some("Succeeded")
        )
    };
    let created = |p: &Pod| p.metadata.creation_timestamp.as_ref().map(|t| t.0);
    let mut candidates: Vec<&Pod> = pods
        .iter()
        .filter(|p| match index {
            Some(i) => pod_index(p) == Some(i),
            None => true,
        })
        .collect();
    // lowest index first (unknown last), then live before terminated, then newest first
    candidates.sort_by(|a, b| {
        pod_index(a)
            .unwrap_or(usize::MAX)
            .cmp(&pod_index(b).unwrap_or(usize::MAX))
            .then(terminated(a).cmp(&terminated(b)))
            .then(created(b).cmp(&created(a)))
            .then(a.metadata.name.cmp(&b.metadata.name))
    });
    candidates.into_iter().next()
}

#[cfg(test)]
mod tests {
    use super::*;
    use k8s_openapi::api::core::v1::PodStatus;
    use k8s_openapi::apimachinery::pkg::apis::meta::v1::{ObjectMeta, OwnerReference, Time};
    use k8s_openapi::jiff::Timestamp;
    use std::collections::BTreeMap;

    fn map(k: &str, v: &str) -> Option<BTreeMap<String, String>> {
        Some(BTreeMap::from([(k.to_string(), v.to_string())]))
    }

    fn job_pod(name: &str, index: &str, phase: &str, secs: i64) -> Pod {
        Pod {
            metadata: ObjectMeta {
                name: Some(name.into()),
                annotations: map("batch.kubernetes.io/job-completion-index", index),
                creation_timestamp: Some(Time(
                    Timestamp::from_second(1_700_000_000 + secs).unwrap(),
                )),
                ..Default::default()
            },
            status: Some(PodStatus {
                phase: Some(phase.into()),
                ..Default::default()
            }),
            ..Default::default()
        }
    }

    fn sts_pod(name: &str, label: Option<&str>) -> Pod {
        Pod {
            metadata: ObjectMeta {
                name: Some(name.into()),
                labels: label.and_then(|l| map("apps.kubernetes.io/pod-index", l)),
                owner_references: Some(vec![OwnerReference {
                    kind: "StatefulSet".into(),
                    name: "x".into(),
                    ..Default::default()
                }]),
                ..Default::default()
            },
            ..Default::default()
        }
    }

    fn name(p: Option<&Pod>) -> Option<&str> {
        p.and_then(|p| p.metadata.name.as_deref())
    }

    #[test]
    fn job_pods_are_found_by_completion_index_not_by_name() {
        let pods = vec![
            job_pod("run-1-xk2p9", "1", "Running", 0),
            job_pod("run-0-abcde", "0", "Running", 0),
        ];
        assert_eq!(name(select_pod(&pods, Some(1))), Some("run-1-xk2p9"));
        assert_eq!(name(select_pod(&pods, Some(0))), Some("run-0-abcde"));
        assert!(select_pod(&pods, Some(2)).is_none());
        // no index: the lowest one, whatever the list order
        assert_eq!(name(select_pod(&pods, None)), Some("run-0-abcde"));
    }

    #[test]
    fn a_replacement_pod_wins_over_the_failed_one_with_the_same_index() {
        let pods = vec![
            job_pod("run-0-old", "0", "Failed", 0),
            job_pod("run-0-new", "0", "Running", 60),
        ];
        assert_eq!(name(select_pod(&pods, Some(0))), Some("run-0-new"));
        // both terminated: the newest
        let pods = vec![
            job_pod("run-0-old", "0", "Failed", 0),
            job_pod("run-0-new", "0", "Succeeded", 60),
        ];
        assert_eq!(name(select_pod(&pods, None)), Some("run-0-new"));
    }

    #[test]
    fn statefulset_pods_use_the_label_or_the_name_suffix() {
        let pods = vec![
            sts_pod("run-training-1", None),
            sts_pod("run-training-0", Some("0")),
        ];
        assert_eq!(name(select_pod(&pods, Some(1))), Some("run-training-1"));
        assert_eq!(name(select_pod(&pods, Some(0))), Some("run-training-0"));
        assert_eq!(name(select_pod(&pods, None)), Some("run-training-0"));
    }

    #[test]
    fn a_pod_without_an_index_is_only_chosen_when_none_is_requested() {
        let plain = Pod {
            metadata: ObjectMeta {
                name: Some("mystery".into()),
                ..Default::default()
            },
            ..Default::default()
        };
        let pods = vec![plain];
        assert_eq!(name(select_pod(&pods, None)), Some("mystery"));
        assert!(select_pod(&pods, Some(0)).is_none());
    }
}
