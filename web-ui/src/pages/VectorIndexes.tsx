// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { formatRelative } from "@/lib/format";
import { notify } from "@/lib/notify";
import { phaseTone } from "@/lib/phase";
import {
  canReingest,
  contentLabel,
  displayPhase,
  retrieveCurl,
  storeLabel,
  type VectorIndex,
} from "@/lib/rag";
import type { SortAccessor } from "@/lib/tableState";
import type { Column } from "@/components/DataTable";
import ResourceListPage from "@/components/ResourceListPage";

const QUERY_KEY = "vector-indexes";

const SORTS: Record<string, SortAccessor<VectorIndex>> = {
  name: (i) => i.metadata.name,
  phase: (i) => displayPhase(i),
  chunks: (i) => i.status.chunks ?? 0,
};

const columns: Column<VectorIndex>[] = [
  {
    key: "name",
    header: "Index",
    sortable: true,
    render: (i) => <span className="mono">{i.metadata.name}</span>,
  },
  { key: "dataset", header: "Dataset", render: (i) => <span className="mono">{i.spec.datasetRef ?? "—"}</span> },
  { key: "model", header: "Embedding model", render: (i) => <span className="mono">{i.spec.embeddingModel ?? "—"}</span> },
  {
    key: "phase",
    header: "Phase",
    sortable: true,
    render: (i) => (
      <span className={`pill ${phaseTone(displayPhase(i))}`} title={i.status.message}>
        {displayPhase(i)}
      </span>
    ),
  },
  {
    key: "chunks",
    header: "Content",
    sortable: true,
    render: (i) => contentLabel(i),
  },
  { key: "version", header: "Dataset version", render: (i) => i.status.datasetVersion ?? "—" },
  {
    key: "ingested",
    header: "Ingested",
    render: (i) => (i.status.lastIngested ? formatRelative(i.status.lastIngested) : "—"),
  },
];

function IndexCard({ index }: { index: VectorIndex }) {
  const queryClient = useQueryClient();
  const name = index.metadata.name;
  const { data: list } = useQuery({
    queryKey: [QUERY_KEY, "retrieve-url"],
    queryFn: api.getVectorIndexes,
    staleTime: 5 * 60 * 1000,
  });
  const done = (msg: string) => {
    notify.success(msg);
    return queryClient.invalidateQueries({ queryKey: [QUERY_KEY] });
  };
  const reingest = useMutation({
    mutationFn: () => api.reingestVectorIndex(name),
    onSuccess: () => done(`Re-ingesting ${name}`),
    onError: (err) => notify.error(`Could not re-ingest ${name}`, err),
  });
  const suspend = useMutation({
    mutationFn: (s: boolean) => api.suspendVectorIndex(name, s),
    onSuccess: (_d, s) => done(s ? `Suspended ${name}` : `Resumed ${name}`),
    onError: (err) => notify.error(`Could not update ${name}`, err),
  });
  const st = index.status;
  const rows: [string, string][] = [
    ["Store", storeLabel(index)],
    ["Store URL", st.storeURL ?? "—"],
    ["Collection", st.collection ?? index.spec.collection ?? name],
    ["Dimensions", st.dimensions ? String(st.dimensions) : "—"],
    ["Chunking", index.spec.chunking ? `${index.spec.chunking.size ?? 1000} chars, overlap ${index.spec.chunking.overlap ?? 0}` : "1000 chars, overlap 200"],
    ["Schedule", index.spec.schedule ?? "—"],
    ["Next run", st.nextScheduleTime ? formatRelative(st.nextScheduleTime) : "—"],
    ["Last ingestion Job", st.ingestJob ?? "—"],
    ["Gateway key Secret", st.keySecret ?? "—"],
  ];
  return (
    <section className="card span3">
      <p className="eyebrow">Vector index</p>
      <h2 className="card-title">
        <span className="mono">{name}</span>
      </h2>
      {st.message && <p>{st.message}</p>}
      <div className="table-wrap">
        <table>
          <caption className="sr-only">Details of {name}</caption>
          <tbody>
            {rows.map(([k, v]) => (
              <tr key={k}>
                <th scope="row">{k}</th>
                <td className="mono">{v}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div style={{ display: "flex", gap: "0.5rem", marginTop: "1rem" }}>
        <button
          type="button"
          className="primary"
          disabled={!canReingest(index) || reingest.isPending}
          onClick={() => reingest.mutate()}
        >
          Re-ingest
        </button>
        <button
          type="button"
          className="btn-secondary"
          disabled={suspend.isPending}
          onClick={() => suspend.mutate(!index.spec.suspend)}
        >
          {index.spec.suspend ? "Resume" : "Suspend"}
        </button>
      </div>
      <p className="eyebrow" style={{ marginTop: "1rem" }}>
        Query it through the LLM gateway (a key of the index's namespace)
      </p>
      <pre className="mono">{retrieveCurl(list?.retrieveURL ?? "", name)}</pre>
    </section>
  );
}

export default function VectorIndexes() {
  return (
    <ResourceListPage<VectorIndex>
      title="Vector indexes"
      noun="vector index"
      eyebrow="RAG"
      heroTitle="Your datasets, retrievable."
      lede="Each index chunks a dataset version, embeds it through the LLM gateway and keeps it in a vector store. Applications ask the gateway's /v1/retrieve for the nearest chunks, with their own API key."
      queryKey={QUERY_KEY}
      fetch={async () => (await api.getVectorIndexes()).items}
      remove={api.deleteVectorIndex}
      columns={columns}
      sorts={SORTS}
      searchText={(i) => `${i.metadata.name} ${i.spec.datasetRef ?? ""} ${i.spec.embeddingModel ?? ""} ${displayPhase(i)}`}
      createHint="gryvia rag index create -f examples/rag/vector-index.yaml"
      deleteNote="Its managed Qdrant (with its volume), ingestion Jobs and gateway key are deleted."
      detail={(i) => <IndexCard index={i} />}
    />
  );
}
