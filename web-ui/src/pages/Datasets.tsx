// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { api } from "@/lib/api";
import { formatBytes, formatRelative } from "@/lib/format";
import { phaseTone } from "@/lib/phase";
import {
  mountLabel,
  sourceLabel,
  versionsNewestFirst,
  type Dataset,
} from "@/lib/datasets";
import type { SortAccessor } from "@/lib/tableState";
import type { Column } from "@/components/DataTable";
import ResourceListPage from "@/components/ResourceListPage";

const SORTS: Record<string, SortAccessor<Dataset>> = {
  name: (d) => d.metadata.name,
  state: (d) => d.status.state ?? "",
  size: (d) => d.status.totalSizeBytes ?? 0,
};

const columns: Column<Dataset>[] = [
  {
    key: "name",
    header: "Dataset",
    sortable: true,
    render: (d) => <span className="mono">{d.metadata.name}</span>,
  },
  { key: "source", header: "Source", render: (d) => sourceLabel(d) },
  {
    key: "state",
    header: "State",
    sortable: true,
    render: (d) => (
      <span className={`pill ${phaseTone(d.status.state)}`} title={d.status.message}>
        {d.status.state ?? "pending"}
      </span>
    ),
  },
  { key: "version", header: "Version", render: (d) => d.status.currentVersion ?? "—" },
  {
    key: "mount",
    header: "PVC",
    render: (d) => <span className="mono">{mountLabel(d)}</span>,
  },
  {
    key: "size",
    header: "Size",
    sortable: true,
    numeric: true,
    render: (d) => formatBytes(d.status.totalSizeBytes),
  },
];

function VersionsCard(d: Dataset) {
  const versions = versionsNewestFirst(d);
  return (
    <section className="card span3">
      <p className="eyebrow">Versions</p>
      <h2 className="card-title">
        Versions of <span className="mono">{d.metadata.name}</span>
      </h2>
      {d.status.message && <p>{d.status.message}</p>}
      {versions.length === 0 ? (
        <p>No version downloaded yet.</p>
      ) : (
        <div className="table-wrap">
          <table>
            <caption className="sr-only">Versions of {d.metadata.name}</caption>
            <thead>
              <tr>
                <th scope="col">Version</th>
                <th scope="col">Downloaded</th>
                <th scope="col">Size</th>
                <th scope="col">sha256</th>
              </tr>
            </thead>
            <tbody>
              {versions.map((v) => (
                <tr key={v.version}>
                  <td className="mono">{v.version}</td>
                  <td>{v.createdAt ? formatRelative(v.createdAt) : "—"}</td>
                  <td>{formatBytes(v.size)}</td>
                  <td className="mono">{v.checksum ? v.checksum.slice(0, 12) : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export default function Datasets() {
  return (
    <ResourceListPage<Dataset>
      title="Datasets"
      noun="dataset"
      eyebrow="Datasets"
      heroTitle="Data, downloaded once, versioned."
      lede="Each dataset's http, s3 or nfs source is downloaded into a PVC, one directory per version. Jobs, RAG ingestion and evaluation in the dataset's namespace mount it."
      queryKey="datasets"
      fetch={api.getDatasets}
      remove={api.deleteDataset}
      columns={columns}
      sorts={SORTS}
      searchText={(d) => `${d.metadata.name} ${sourceLabel(d)} ${d.status.state ?? ""}`}
      createHint="gryvia datasets create -f examples/datasets/http-dataset.yaml"
      deleteNote="Its PVC and every downloaded version are deleted."
      detail={VersionsCard}
    />
  );
}
