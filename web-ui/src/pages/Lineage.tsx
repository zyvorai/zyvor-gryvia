// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { KIND_LABEL, columns, reach, type LineageNode } from '@/lib/lineage'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import PageHero from '@/components/PageHero'
import { ErrorState, Skeleton } from '@/components/StateViews'

export default function Lineage() {
  useDocumentTitle('Lineage')
  const q = useQuery({ queryKey: ['lineage'], queryFn: api.getLineage, refetchInterval: 30000 })
  const [selected, setSelected] = useState<string | null>(null)
  const hero = (
    <PageHero
      eyebrow="Lineage"
      title="From dataset to served model."
      lede="Which data trained which model, and what serves it. Select a node to highlight everything upstream and downstream."
    />
  )
  if (q.isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={4} />
      </>
    )
  }
  if (q.isError && !q.data) {
    return (
      <>
        {hero}
        <ErrorState title="Could not load the lineage graph." error={q.error} onRetry={() => q.refetch()} retrying={q.isRefetching} />
      </>
    )
  }
  const graph = q.data ?? { nodes: [], edges: [] }
  const related = new Set<string>()
  if (selected) {
    related.add(selected)
    for (const dir of ['up', 'down'] as const) reach(graph, selected, dir).forEach((n) => related.add(n.id))
  }
  const detail = (n: LineageNode) => n.state ?? n.phase ?? [n.version, n.stage].filter(Boolean).join(' · ')

  return (
    <>
      {hero}
      {graph.nodes.length === 0 ? (
        <p className="muted">Nothing to show yet. Create a dataset, run a job on it and register the result as a model.</p>
      ) : (
        <div className="grid">
          {columns(graph).map((col) => (
            <section className="card" key={col.kind} aria-label={KIND_LABEL[col.kind]}>
              <p className="eyebrow">{KIND_LABEL[col.kind]}</p>
              <ul className="stack">
                {col.nodes.map((n) => (
                  <li key={n.id}>
                    <button
                      type="button"
                      className={`btn ${selected === n.id ? 'primary' : ''}`}
                      aria-pressed={selected === n.id}
                      style={selected && !related.has(n.id) ? { opacity: 0.4 } : undefined}
                      onClick={() => setSelected(selected === n.id ? null : n.id)}
                    >
                      <span className="mono">{n.name}</span>
                      {detail(n) && <span className="muted"> {detail(n)}</span>}
                    </button>
                  </li>
                ))}
                {col.nodes.length === 0 && <li className="muted">None</li>}
              </ul>
            </section>
          ))}
        </div>
      )}
    </>
  )
}
