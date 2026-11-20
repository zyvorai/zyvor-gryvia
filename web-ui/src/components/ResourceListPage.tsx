// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { notify } from "@/lib/notify";
import type { SortAccessor } from "@/lib/tableState";
import { useDocumentTitle } from "@/hooks/useDocumentTitle";
import { useTableState } from "@/hooks/useTableState";
import DataTable, { type Column } from "@/components/DataTable";
import PageHero from "@/components/PageHero";
import ConfirmDialog from "@/components/ConfirmDialog";
import { EmptyState, ErrorState, Skeleton } from "@/components/StateViews";

export interface Named {
  metadata: { name: string; namespace?: string };
}

interface Props<T extends Named> {
  /** Page title and the plural noun ("Datasets"). */
  title: string;
  /** Singular noun for messages ("dataset"). */
  noun: string;
  eyebrow: string;
  heroTitle: string;
  lede: string;
  queryKey: string;
  fetch: () => Promise<T[]>;
  remove: (name: string) => Promise<void>;
  columns: Column<T>[];
  sorts: Record<string, SortAccessor<T>>;
  searchText: (row: T) => string;
  /** Shown in the empty state: how to create one. */
  createHint: string;
  /** What deleting also removes. */
  deleteNote: string;
  /** Optional card under the table for the selected row. */
  detail?: (row: T) => ReactNode;
}

/** A list page for one kind: searchable table, delete with confirmation, optional detail card. */
export default function ResourceListPage<T extends Named>(props: Props<T>) {
  useDocumentTitle(props.title);
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: [props.queryKey],
    queryFn: props.fetch,
    refetchInterval: 30000,
  });
  const table = useTableState({
    rows: data,
    searchText: props.searchText,
    sortAccessors: props.sorts,
    defaultSort: { key: "name", dir: "asc" },
    pageSize: 12,
  });
  const remove = useMutation({
    mutationFn: (name: string) => props.remove(name),
    onSuccess: (_d, name) => {
      notify.success(`Deleted ${name}`);
      setDeleting(null);
      if (selected === name) setSelected(null);
      return queryClient.invalidateQueries({ queryKey: [props.queryKey] });
    },
    onError: (err, name) => {
      notify.error(`Could not delete ${name}`, err);
      setDeleting(null);
    },
  });

  const hero = (
    <PageHero eyebrow={props.eyebrow} title={props.heroTitle} lede={props.lede} />
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
          eyebrow={props.eyebrow}
          title={`${props.title} unavailable.`}
          tint="red"
        />
        <ErrorState
          title={`Could not load ${props.title.toLowerCase()}.`}
          error={error}
          onRetry={() => refetch()}
          retrying={isRefetching}
        />
      </>
    );
  }

  const columns: Column<T>[] = [
    ...props.columns,
    {
      key: "actions",
      header: "Actions",
      render: (row) => (
        <div className="toolbar" onClick={(e) => e.stopPropagation()}>
          <button
            type="button"
            className="btn-secondary"
            onClick={() => setDeleting(row.metadata.name)}
          >
            Delete
          </button>
        </div>
      ),
    },
  ];
  const chosen = selected
    ? data?.find((r) => r.metadata.name === selected)
    : undefined;

  return (
    <>
      {hero}
      <div className="grid">
        {isError && (
          <div className="span3">
            <ErrorState
              title={`Could not refresh ${props.title.toLowerCase()}; showing the last data.`}
              error={error}
              onRetry={() => refetch()}
              retrying={isRefetching}
            />
          </div>
        )}
        <section className="card span3">
          <p className="eyebrow">{props.eyebrow}</p>
          <h2 className="card-title">{props.title}</h2>
          <DataTable
            caption={props.title}
            columns={columns}
            state={table}
            rowKey={(r) => `${r.metadata.namespace ?? ""}/${r.metadata.name}`}
            onRowClick={props.detail ? (r) => setSelected(r.metadata.name) : undefined}
            searchLabel={`Search ${props.title.toLowerCase()}`}
            empty={
              <EmptyState title={`No ${props.title.toLowerCase()}.`}>
                Create one from YAML: <span className="mono">{props.createHint}</span>
              </EmptyState>
            }
          />
        </section>
        {chosen && props.detail && props.detail(chosen)}
      </div>
      {deleting && (
        <ConfirmDialog
          title={`Delete ${props.noun} ${deleting}?`}
          confirmLabel={`Delete ${props.noun}`}
          busy={remove.isPending}
          onCancel={() => setDeleting(null)}
          onConfirm={() => remove.mutate(deleting)}
        >
          {props.deleteNote}
        </ConfirmDialog>
      )}
    </>
  );
}
