# Datasets (GryviaDataset)

A `GryviaDataset` names a data source and the storage-operator materializes it into a PVC, one directory per
version, so training jobs, RAG ingestion and evaluation read the same bytes. It is opt-in.

## What is verified and what is not

| Verified | How |
| --- | --- |
| PVC creation (size, storage class, owner), the download Job for each source type (image, env, volumes, non-root pod), the termination-message result read back into status, versions and `keepLast` retention, a changed source downloading again, failures and invalid specs reported in status | Unit tests with a fake client (`operators/storage-operator/controllers/gryviadataset_controller_test.go`) |
| The download script's resume and incremental paths: a partial http file resumed with and without server Range support, a complete one (416), a checksum mismatch keeping the temporary directory, another source's directory discarded, and an s3 version that fetches only changed objects while unchanged ones stay hard-linked and the old version is untouched | The script run under busybox (2026-10-02) with a stand-in server and a fake `aws` |
| The download script (http with sha256, the swap into `/data/<version>`, pruning, the file count, size and checksum) on a real cluster | The "Datasets" step of `.github/workflows/e2e-ml.yml` with a stand-in HTTP server; these steps also passed on a single-node k3s host (2026-10-01) |
| s3 with the real AWS CLI image (`amazon/aws-cli:2.17.0`) against an S3-compatible server through `source.s3.endpoint`: a prefix with nested keys downloaded, then a new version after one object changed, one was deleted and one was added. The new version has exactly the current objects, the unchanged object is the same inode as in the old version (hard-linked, not downloaded again), the changed one is a new file, and the old version is untouched | The "Datasets - s3 source" step of `.github/workflows/e2e-ml.yml` with versitygw (posix backend) as the server; passed in kind CI on main ([run 36990962579](https://github.com/zyvorai/gryvia/actions/runs/36990962579), 2026-10-02) |
| nfs on a real cluster: the export mounted read-only by the download Job, nested files copied, a second version after the export changed, retention | The "Datasets - nfs source" step of `.github/workflows/e2e-ml.yml`: a kernel NFS server on the CI runner, the NFS client installed in the kind node; passed in kind CI on main ([run 36990962579](https://github.com/zyvorai/gryvia/actions/runs/36990962579), 2026-10-02) |
| Gateway routes and tenant scoping | `services/api-gateway/tests/test_datasets.py` |
| Pool placement: replica PVCs and pool-pinned download Jobs, replicas waiting for the primary (or starting with it under `cache.warmup`), digest verification and a mismatch reported, removed pools pruned, invalid placement reported; on the AI operator side the tie broken toward a pool with a verified replica, the read-only mount, and the cases that do not mount (multi-node with ReadWriteOnce, another namespace, nothing verified, dataset missing) | Unit tests with fake clients (`operators/storage-operator/controllers/dataset_placement_test.go`, `operators/ai-operator/controllers/gryviaaijob_dataset_test.go`) |

| Not verified | Why |
| --- | --- |
| Pool placement on a real multi-node cluster with a node-local storage class | Not run yet; the lab host is a single node |
| Amazon S3 itself, large data, and resuming an interrupted s3 sync | CI uses versitygw with a few small objects; an interrupted sync was only run with a fake `aws` under busybox |
| NFS servers other than the Linux kernel server, Kerberos or root-squashed exports, large copies | CI exports a few small files read-only with `all_squash` |

## Turning it on

```yaml
storageOperator:
  enabled: true
  datasets:
    enabled: true          # --enable-datasets
    namespace: ""          # --dataset-namespace: default for spec.namespace (empty: the chart's namespace)
    image: busybox:1.36    # http and nfs Jobs: sh, wget, sha256sum, find, stat
    s3Image: amazon/aws-cli:2.17.0
    defaultSize: 10Gi      # PVC size without spec.cache.size
```

Without `--enable-datasets` and with `platformCompletion.reportUnsupportedAPIs`, datasets keep getting
`Ready=False, reason UnsupportedAPI`, as before.

## Spec

The kind is cluster-scoped. The fields the controller reads:

| Field | Meaning |
| --- | --- |
| `source.type` | `http`, `s3` or `nfs` (`gcs`, `azure-blob`, `git-lfs` and `vast` are rejected in status) |
| `source.http.url`, `source.http.checksumURL` | One file. With `checksumURL` the first field of its first line must equal the sha256 of the file, or the download fails |
| `source.s3.bucket`, `prefix`, `region`, `credentialsSecret` | `aws s3 sync s3://bucket/prefix`. The Secret, in the dataset namespace, holds `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` |
| `source.s3.endpoint` | URL of an S3-compatible service (MinIO, Ceph RGW, Cloudflare R2, versitygw, ...), passed as `AWS_ENDPOINT_URL`; empty means AWS. Set `region` too (`us-east-1` for most of them) |
| `source.nfs.server`, `source.nfs.path` | The export is mounted read-only and copied |
| `version` | Directory name of this version (default `latest`; letters, digits, `.`, `_`, `-`) |
| `namespace` | Where the PVC and Jobs live (default `--dataset-namespace`); consumers run there |
| `cache.size`, `cache.storageClass` | The PVC (ReadWriteOnce) |
| `placement.pools[]` (`name`, `nodeSelector`), `placement.storageClass`, `placement.accessMode` | One extra copy of the current version per pool; see [Pool placement](#pool-placement) |
| `cache.warmup` | Start the pool replica downloads together with the primary instead of after it |
| `versioning.enabled`, `versioning.retentionPolicy.keepLast` | Without versioning only the current version is kept; with it the newest `keepLast` (all when 0) |

`access`, `statistics`, `tags`, `license` and `description` are stored and shown, not enforced.

## What the controller does

1. Creates PVC `dataset-<name>` in the namespace, owned by the dataset (deleting the dataset deletes the data).
2. Hashes the source and version. When they differ from `status.sourceHash`, it runs Job `dataset-<name>-<hash>`
   (non-root, all capabilities dropped, 2 retries, deleted 24h after it finishes). The Job downloads into
   `/data/.tmp-<version>`, swaps it into `/data/<version>`, removes version directories that retention no longer
   keeps, and writes `{"files","bytes","sha256"}` to its termination message (the sha256 is over the sorted
   per-file checksums). Downloads are not repeated needlessly:
   - **Resume.** A failed attempt leaves the temporary directory, tagged with the source hash. The next attempt of
     the same source continues it: http resumes the partial file with `wget -c` (and downloads it afresh when the
     server does not support ranges), s3 fetches only what is missing. A checksum mismatch deletes the file. A
     temporary directory left by another source is discarded.
   - **Incremental s3.** A new version's directory starts as hard links to the current version's files, and
     `aws s3 sync --delete` then downloads only new and changed objects and removes deleted ones. The AWS CLI
     writes each download to a temporary file and renames it, so the old version's files are not modified.
   - nfs copies the whole export each time.
3. On success: `status.state: ready`, `currentVersion`, `subPath` (the version directory), `fileCount`,
   `totalSizeBytes`, and an entry in `versions` with the size and checksum. On failure: `state: error` and the
   reason in `message` (a checksum mismatch, for example). Condition `Ready` mirrors the state.

A job reads the current version with:

```yaml
volumes: [{name: data, persistentVolumeClaim: {claimName: dataset-corpus}}]
volumeMounts: [{name: data, mountPath: /data, subPath: v1, readOnly: true}]
```

The PVC is ReadWriteOnce: on a multi-node cluster its readers have to run on the node that mounted it, or use a
storage class with ReadWriteMany.

## Pool placement

`spec.placement` keeps a copy of the current version in each listed node pool, so jobs read local data:

```yaml
spec:
  placement:
    storageClass: local-path         # node-local, volumeBindingMode WaitForFirstConsumer
    accessMode: ReadWriteOnce        # default; ReadOnlyMany / ReadWriteMany for shared pool storage
    pools:
      - {name: zone-a, nodeSelector: {topology.kubernetes.io/zone: a}}
      - {name: rack-7, nodeSelector: {gryvia.io/rack: r7}}
```

- Each pool gets PVC `dataset-<name>-<pool>` and its own download Job (`dataset-<name>-<pool>-<hash>`) with the
  pool's `nodeSelector`, so a WaitForFirstConsumer class binds the volume inside the pool. The Job runs the same
  script as the primary download (resume, checksum) and keeps only the current version.
- Replica downloads start once the primary copy is ready, or right away with `cache.warmup: true`.
- A replica is **verified** when its digest (sha256 over the sorted per-file checksums) equals the primary's. A
  mismatch (the source changed between the downloads) is reported in the replica's `message` and the replica is
  not used. `status.replicas[]` reports `pool`, `pvcName`, `version`, `digest`, `ready`, `verified`, `files`,
  `bytes`, `lastSynced`; condition `Placed` is True when every replica is verified.
- Removing a pool from the list deletes its replica PVC. Every replica re-downloads when the source or version
  changes.

### Jobs that use a dataset

Annotate a `GryviaAIJob` with `gryvia.io/dataset: <name>`. When it is scheduled, the AI operator reads the
dataset's verified replicas of the current version and:

- ranks eligible nodes in those pools first (ties only; it never makes an ineligible node eligible) and adds soft
  preferred node affinity (weight 50) toward them;
- mounts the replica of the chosen node's pool read-only at `/datasets/<name>` (version subPath) and sets
  `GRYVIA_DATASET_PATH`, when the job runs in the dataset's namespace and either runs on a single node or the
  replica is ReadOnlyMany / ReadWriteMany. A ReadWriteOnce volume can only be attached on one node.

The decision is made once and recorded in `status.dataset` (`localPools`, `pool`, `pvcName`, `subPath`,
`message`). A missing dataset, no verified replica or a lookup error leaves scheduling unchanged.

## Surfaces

* Gateway: `GET /api/datasets`, `GET /api/datasets/{name}`, `POST /api/datasets`, `DELETE /api/datasets/{name}`.
  A tenant sees and deletes only datasets whose namespace is one of theirs; a tenant's new dataset goes to their
  first namespace (another namespace is 403).
* CLI: `gryvia datasets list|get|create -f|delete`.
* Dashboard: the "Datasets" page under Models.

Example: [examples/datasets/http-dataset.yaml](../examples/datasets/http-dataset.yaml).
