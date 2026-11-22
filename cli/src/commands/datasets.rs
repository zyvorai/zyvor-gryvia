// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//! `gryvia datasets list|get|create|delete`: GryviaDataset objects (cluster-scoped). The storage-operator
//! (with --enable-datasets) downloads each dataset's source into a PVC in `spec.namespace`, one directory per
//! version (see docs/datasets.md).

use crate::commands::crd::{state_marker, Column, KindSpec};

pub const DATASETS: KindSpec = KindSpec {
    kind: "GryviaDataset",
    plural: "gryviadatasets",
    cluster: true,
    noun: "dataset",
    title: "Datasets",
    columns: &[
        Column {
            header: "NAME",
            ptr: "/metadata/name",
            marker: false,
        },
        Column {
            header: "SOURCE",
            ptr: "/spec/source/type",
            marker: false,
        },
        Column {
            header: "STATE",
            ptr: "/status/state",
            marker: true,
        },
        Column {
            header: "VERSION",
            ptr: "/status/currentVersion",
            marker: false,
        },
        Column {
            header: "NAMESPACE",
            ptr: "/status/namespace",
            marker: false,
        },
        Column {
            header: "PVC",
            ptr: "/status/pvcName",
            marker: false,
        },
        Column {
            header: "FILES",
            ptr: "/status/fileCount",
            marker: false,
        },
        Column {
            header: "BYTES",
            ptr: "/status/totalSizeBytes",
            marker: false,
        },
    ],
    marker: state_marker,
};
