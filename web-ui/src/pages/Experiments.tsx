import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import {
  directionLabel,
  experimentName,
  gapLabel,
  leaderOf,
  type Experiment,
} from "@/lib/experiments";
import { formatDate, formatMoney, formatNumber } from "@/lib/format";
import { phaseTone } from "@/lib/phase";
import type { SortAccessor } from "@/lib/tableState";
import { useDocumentTitle } from "@/hooks/useDocumentTitle";
import { useTableState } from "@/hooks/useTableState";
import DataTable, { type Column } from "@/components/DataTable";
import PageHero from "@/components/PageHero";
import { EmptyState, ErrorState, Skeleton } from "@/components/StateViews";

const phaseOf = (e: Experiment) => e.status?.phase || "Pending";
const SORTS: Record<string, SortAccessor<Experiment>> = {
  name: experimentName,
  metric: (e) => e.spec?.primaryMetric ?? "",
  jobs: (e) => e.spec?.jobs?.length ?? 0,
  best: (e) => leaderOf(e)?.primaryMetricValue,
  status: phaseOf,
  started: (e) =>
    e.status?.startedAt ? Date.parse(e.status.startedAt) : undefined,
};
const searchText = (e: Experiment) =>
  `${experimentName(e)} ${e.spec?.primaryMetric ?? ""} ${phaseOf(e)}`;

export default function Experiments() {
  useDocumentTitle("Experiments");
  const [open, setOpen] = useState<string | null>(null);
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["experiments"],
    queryFn: api.getExperiments,
    refetchInterval: 30000,
  });
  const table = useTableState({
    rows: data,
    searchText,
    sortAccessors: SORTS,
    defaultSort: { key: "started", dir: "desc" },
    pageSize: 12,
  });

  const hero = (
    <PageHero
      eyebrow="Experiments"
      title="Experiments."
      lede="Live leaderboards for runs compared on one metric (Determined-style visibility without locking a training API). Ranks follow the experiment's direction; the gap is to the best run. Pair with GryviaTrainingProfiler CRDs for GPU util traces."
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
          eyebrow="Experiments"
          title="Experiments unavailable."
          tint="red"
        />
        <ErrorState
          title="Could not load experiments."
          error={error}
          onRetry={() => refetch()}
          retrying={isRefetching}
        />
      </>
    );
  }

  const columns: Column<Experiment>[] = [
    {
      key: "name",
      header: "Experiment",
      sortable: true,
      render: (e) => (
        <button
          type="button"
          className="link mono"
          onClick={() =>
            setOpen(open === experimentName(e) ? null : experimentName(e))
          }
          aria-expanded={open === experimentName(e)}
        >
          {experimentName(e)}
        </button>
      ),
    },
    {
      key: "metric",
      header: "Metric",
      sortable: true,
      render: (e) =>
        `${e.spec?.primaryMetric ?? "—"} (${directionLabel(e.spec?.direction)})`,
    },
    {
      key: "jobs",
      header: "Runs",
      sortable: true,
      numeric: true,
      render: (e) => formatNumber(e.spec?.jobs?.length ?? 0),
    },
    { key: "leader", header: "Leader", render: (e) => leaderOf(e)?.job ?? "—" },
    {
      key: "best",
      header: "Best",
      sortable: true,
      numeric: true,
      render: (e) =>
        leaderOf(e)?.primaryMetricValue !== undefined
          ? formatNumber(leaderOf(e)!.primaryMetricValue!)
          : "—",
    },
    {
      key: "status",
      header: "Status",
      sortable: true,
      render: (e) => (
        <span className={`pill ${phaseTone(phaseOf(e))}`}>{phaseOf(e)}</span>
      ),
    },
    {
      key: "started",
      header: "Started",
      sortable: true,
      render: (e) =>
        e.status?.startedAt ? formatDate(e.status.startedAt) : "—",
    },
  ];

  const selected = (data ?? []).find((e) => experimentName(e) === open);

  return (
    <>
      {hero}
      <div className="grid">
        {isError && (
          <div className="span3">
            <ErrorState
              title="Could not refresh experiments; showing the last data."
              error={error}
              onRetry={() => refetch()}
              retrying={isRefetching}
            />
          </div>
        )}
        <section className="card span3">
          <p className="eyebrow">Experiments</p>
          <h2 className="card-title">Runs compared on one metric</h2>
          <DataTable
            caption="Experiments"
            columns={columns}
            state={table}
            rowKey={experimentName}
            searchLabel="Search experiments"
            empty={
              <EmptyState title="No experiments.">
                Create a GryviaLiveExperiment that references your jobs to
                compare them here.
              </EmptyState>
            }
          />
        </section>
        {selected && (
          <section className="card span3">
            <p className="eyebrow">Leaderboard</p>
            <h2 className="card-title">{experimentName(selected)}</h2>
            {selected.spec?.description && (
              <p className="faint">{selected.spec.description}</p>
            )}
            <table
              className="table"
              aria-label={`Leaderboard of ${experimentName(selected)}`}
            >
              <thead>
                <tr>
                  <th>#</th>
                  <th>Run</th>
                  <th className="num">
                    {selected.spec?.primaryMetric ?? "Metric"}
                  </th>
                  <th>Gap to best</th>
                  <th>Status</th>
                </tr>
              </thead>
              <tbody>
                {(selected.status?.leaderboard ?? []).map((r) => (
                  <tr key={r.job}>
                    <td>{r.rank ?? "—"}</td>
                    <td className="mono">{r.job}</td>
                    <td className="num">
                      {r.primaryMetricValue !== undefined
                        ? formatNumber(r.primaryMetricValue)
                        : "—"}
                    </td>
                    <td>{gapLabel(r)}</td>
                    <td title={r.reason}>{r.status ?? "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {(selected.status?.gpuHoursSaved !== undefined ||
              selected.status?.costSaved !== undefined) && (
              <p className="faint">
                Early termination saved{" "}
                {formatNumber(selected.status?.gpuHoursSaved ?? 0)} GPU hours (
                {formatMoney(selected.status?.costSaved ?? 0)}), an estimate
                from the operator.
              </p>
            )}
          </section>
        )}
      </div>
    </>
  );
}
