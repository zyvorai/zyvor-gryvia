// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { formatRelative } from "@/lib/format";
import { notify } from "@/lib/notify";
import { phaseTone } from "@/lib/phase";
import {
  candidateCount,
  shortRevision,
  sourcesLabel,
  visibleRuns,
  watchTone,
  type ModelWatch,
  type ModelWatchRun,
} from "@/lib/modelWatches";
import type { SortAccessor } from "@/lib/tableState";
import { useDocumentTitle } from "@/hooks/useDocumentTitle";
import { useTableState } from "@/hooks/useTableState";
import DataTable, { type Column } from "@/components/DataTable";
import PageHero from "@/components/PageHero";
import ConfirmDialog from "@/components/ConfirmDialog";
import { EmptyState, ErrorState, Skeleton } from "@/components/StateViews";

const SORTS: Record<string, SortAccessor<ModelWatch>> = {
  name: (w) => w.metadata.name,
  phase: (w) => w.status.phase ?? "",
  active: (w) => w.status.activeRuns ?? 0,
};
const searchText = (w: ModelWatch) =>
  `${w.metadata.name} ${sourcesLabel(w)} ${w.status.phase ?? ""}`;

export default function ModelFactory() {
  useDocumentTitle("Model factory");
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["model-watches"],
    queryFn: api.getModelWatches,
    refetchInterval: 30000,
  });
  const table = useTableState({
    rows: data,
    searchText,
    sortAccessors: SORTS,
    defaultSort: { key: "name", dir: "asc" },
    pageSize: 12,
  });
  const refresh = () =>
    queryClient.invalidateQueries({ queryKey: ["model-watches"] });

  const suspend = useMutation({
    mutationFn: ({ name, on }: { name: string; on: boolean }) =>
      on ? api.suspendModelWatch(name) : api.resumeModelWatch(name),
    onSuccess: (_d, { name, on }) => {
      notify.success(on ? `Suspended ${name}` : `Resumed ${name}`);
      return refresh();
    },
    onError: (err, { name }) => notify.error(`Could not update ${name}`, err),
  });
  const remove = useMutation({
    mutationFn: (name: string) => api.deleteModelWatch(name),
    onSuccess: (_d, name) => {
      notify.success(`Deleted ${name}`);
      setDeleting(null);
      if (selected === name) setSelected(null);
      return refresh();
    },
    onError: (err, name) => {
      notify.error(`Could not delete ${name}`, err);
      setDeleting(null);
    },
  });

  const hero = (
    <PageHero
      eyebrow="Model factory"
      title="New open models, fine-tuned and served."
      lede="Each watch polls a model hub. A new model that passes the license and size filters gets a workflow: download, fine-tune, evaluate, register. A better score is promoted and canaried onto the shared inference service."
    />
  );
  if (isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={4} />
      </>
    );
  }
  if (isError && !data) {
    return (
      <>
        <PageHero
          eyebrow="Model factory"
          title="Model watches unavailable."
          tint="red"
        />
        <ErrorState
          title="Could not load model watches."
          error={error}
          onRetry={() => refetch()}
          retrying={isRefetching}
        />
      </>
    );
  }

  const columns: Column<ModelWatch>[] = [
    {
      key: "name",
      header: "Watch",
      sortable: true,
      render: (w) => <span className="mono">{w.metadata.name}</span>,
    },
    { key: "sources", header: "Sources", render: (w) => sourcesLabel(w) },
    {
      key: "phase",
      header: "Phase",
      sortable: true,
      render: (w) => (
        <span className={`pill ${watchTone(w.status.phase)}`}>
          {w.status.phase ?? "unknown"}
        </span>
      ),
    },
    {
      key: "active",
      header: "Active runs",
      sortable: true,
      numeric: true,
      render: (w) => w.status.activeRuns ?? 0,
    },
    {
      key: "done",
      header: "Succeeded / failed",
      numeric: true,
      render: (w) =>
        `${candidateCount(w, "Succeeded")} / ${candidateCount(w, "Failed")}`,
    },
    {
      key: "poll",
      header: "Last poll",
      render: (w) =>
        w.status.lastPollTime ? formatRelative(w.status.lastPollTime) : "—",
    },
    {
      key: "actions",
      header: "Actions",
      render: (w) => {
        const name = w.metadata.name;
        const busy = suspend.isPending && suspend.variables?.name === name;
        return (
          <div className="toolbar" onClick={(e) => e.stopPropagation()}>
            <button
              type="button"
              className="btn-secondary"
              disabled={busy}
              onClick={() => suspend.mutate({ name, on: !w.spec.suspend })}
            >
              {w.spec.suspend ? "Resume" : "Suspend"}
            </button>
            <button
              type="button"
              className="btn-secondary"
              onClick={() => setDeleting(name)}
            >
              Delete
            </button>
          </div>
        );
      },
    },
  ];

  return (
    <>
      {hero}
      <div className="grid">
        {isError && (
          <div className="span3">
            <ErrorState
              title="Could not refresh model watches; showing the last data."
              error={error}
              onRetry={() => refetch()}
              retrying={isRefetching}
            />
          </div>
        )}
        <section className="card span3">
          <p className="eyebrow">Watches</p>
          <h2 className="card-title">Model watches</h2>
          <DataTable
            caption="Model watches"
            columns={columns}
            state={table}
            rowKey={(w) => w.metadata.name}
            onRowClick={(w) => setSelected(w.metadata.name)}
            searchLabel="Search model watches"
            empty={
              <EmptyState title="No model watches.">
                Create one from YAML:{" "}
                <span className="mono">
                  gryvia models watch create -f
                  examples/model-factory/model-watch.yaml
                </span>
              </EmptyState>
            }
          />
        </section>
        {selected && <RunsCard name={selected} />}
      </div>
      {deleting && (
        <ConfirmDialog
          title={`Delete model watch ${deleting}?`}
          confirmLabel="Delete watch"
          busy={remove.isPending}
          onCancel={() => setDeleting(null)}
          onConfirm={() => remove.mutate(deleting)}
        >
          Its workflows are deleted too. Registered models and inference
          services are kept.
        </ConfirmDialog>
      )}
    </>
  );
}

function RunsCard({ name }: { name: string }) {
  const [showBaseline, setShowBaseline] = useState(false);
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["model-watch-runs", name],
    queryFn: () => api.getModelWatchRuns(name),
    refetchInterval: 30000,
  });
  const runs = visibleRuns(data ?? [], showBaseline);

  return (
    <section className="card span3">
      <p className="eyebrow">Runs</p>
      <h2 className="card-title">
        Models found by <span className="mono">{name}</span>
      </h2>
      <div className="toolbar">
        <label>
          <input
            type="checkbox"
            checked={showBaseline}
            onChange={(e) => setShowBaseline(e.target.checked)}
          />{" "}
          Show baseline models
        </label>
      </div>
      {isLoading && <Skeleton rows={3} />}
      {isError && (
        <ErrorState
          title="Could not load runs."
          error={error}
          onRetry={() => refetch()}
          retrying={isRefetching}
        />
      )}
      {!isLoading && !isError && runs.length === 0 && (
        <EmptyState title="No runs yet.">
          New models appear here after the next poll.
        </EmptyState>
      )}
      {runs.length > 0 && (
        <div className="table-wrap">
          <table>
            <caption className="sr-only">Runs of {name}</caption>
            <thead>
              <tr>
                <th scope="col">Model</th>
                <th scope="col">Revision</th>
                <th scope="col">Params (B)</th>
                <th scope="col">GPUs</th>
                <th scope="col">Phase</th>
                <th scope="col">Workflow / reason</th>
                <th scope="col">First seen</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((r: ModelWatchRun) => (
                <tr key={`${r.model}@${r.revision ?? ""}`}>
                  <td className="mono">{r.model}</td>
                  <td className="mono">{shortRevision(r.revision)}</td>
                  <td>{r.paramsB ?? "—"}</td>
                  <td>{r.gpus ?? "—"}</td>
                  <td>
                    <span className={`pill ${phaseTone(r.phase)}`}>
                      {r.phase ?? "unknown"}
                    </span>
                  </td>
                  <td>
                    {r.workflow ? (
                      <span className="mono">{r.workflow}</span>
                    ) : (
                      (r.message ?? "—")
                    )}
                  </td>
                  <td>{r.firstSeen ? formatRelative(r.firstSeen) : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
