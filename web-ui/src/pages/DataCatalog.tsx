// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { notify } from '@/lib/notify'
import { formatBytes, formatRelative } from '@/lib/format'
import { phaseTone } from '@/lib/phase'
import { formatColumns, parseColumns, parseTags, usedByLabel, type CatalogEntry } from '@/lib/dataCatalog'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import PageHero from '@/components/PageHero'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

function Detail({ entry, onClose }: { entry: CatalogEntry; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [owner, setOwner] = useState(entry.owner)
  const [description, setDescription] = useState(entry.description)
  const [tags, setTags] = useState(entry.tags.join(', '))
  const [columns, setColumns] = useState(formatColumns(entry.columns))
  const save = useMutation({
    mutationFn: () => api.editCatalogEntry(entry.name, { owner, description, tags: parseTags(tags), columns: parseColumns(columns) }),
    onSuccess: () => {
      notify.success(`Saved ${entry.name}`)
      queryClient.invalidateQueries({ queryKey: ['data-catalog'] })
      setEditing(false)
    },
    onError: (err) => notify.error(`Could not save ${entry.name}`, err),
  })

  return (
    <section className="card span3" aria-labelledby="dc-detail">
      <p className="eyebrow">Dataset</p>
      <h2 className="card-title" id="dc-detail">
        <span className="mono">{entry.name}</span>
      </h2>
      {editing ? (
        <form
          style={{ display: 'grid', gap: '0.75rem' }}
          onSubmit={(e) => {
            e.preventDefault()
            save.mutate()
          }}
        >
          <label>
            Owner
            <input value={owner} maxLength={100} onChange={(e) => setOwner(e.target.value)} placeholder="team or person" />
          </label>
          <label>
            Description
            <textarea rows={2} className="codeedit compact" maxLength={1024} value={description} onChange={(e) => setDescription(e.target.value)} />
          </label>
          <label>
            Tags
            <input value={tags} onChange={(e) => setTags(e.target.value)} placeholder="pii, finance (comma separated, up to 10)" />
          </label>
          <label>
            Columns
            <textarea rows={6} className="codeedit compact" value={columns} onChange={(e) => setColumns(e.target.value)} placeholder={'one per line:\nid: int\nemail: string - contact address'} />
          </label>
          {save.isError && (
            <p className="warning" role="alert">
              {save.error instanceof Error ? save.error.message : 'Could not save'}
            </p>
          )}
          <div style={{ display: 'flex', gap: '0.5rem' }}>
            <button type="submit" className="primary" disabled={save.isPending}>
              {save.isPending ? 'Saving…' : 'Save'}
            </button>
            <button type="button" className="btn-secondary" onClick={() => setEditing(false)}>
              Cancel
            </button>
          </div>
        </form>
      ) : (
        <>
          <p>{entry.description || <span className="muted">No description.</span>}</p>
          <p className="muted">
            Owner: {entry.owner || '—'} · Namespace: {entry.namespace} · Version: {entry.version ?? '—'} · {formatBytes(entry.bytes)} · Updated {formatRelative(entry.updated)}
            {entry.license ? ` · License: ${entry.license}` : ''}
          </p>
          <p>
            <strong>Used by:</strong> {usedByLabel(entry.usedBy)}
            {entry.usedBy.length > 0 && <span className="muted"> ({entry.usedBy.join(', ')})</span>}
          </p>
          {entry.columns.length > 0 ? (
            <div className="table-wrap">
              <table>
                <caption className="sr-only">Columns of {entry.name}</caption>
                <thead>
                  <tr>
                    <th>Column</th>
                    <th>Type</th>
                    <th>Description</th>
                  </tr>
                </thead>
                <tbody>
                  {entry.columns.map((c) => (
                    <tr key={c.name}>
                      <td className="mono">{c.name}</td>
                      <td className="mono">{c.type ?? '—'}</td>
                      <td>{c.description ?? ''}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="muted">No schema documented yet.</p>
          )}
          <div style={{ display: 'flex', gap: '0.5rem', marginTop: '0.5rem' }}>
            <button type="button" className="btn-secondary" onClick={() => setEditing(true)}>
              Edit details
            </button>
            <button type="button" className="btn-secondary" onClick={onClose}>
              Close
            </button>
          </div>
        </>
      )}
    </section>
  )
}

export default function DataCatalog() {
  useDocumentTitle('Data catalog')
  const [q, setQ] = useState('')
  const [tag, setTag] = useState('')
  const [owner, setOwner] = useState('')
  const [selected, setSelected] = useState<string | null>(null)
  const query = useQuery({
    queryKey: ['data-catalog', q, tag, owner],
    queryFn: () => api.getDataCatalog({ q, tag, owner }),
    placeholderData: (prev) => prev,
  })

  const hero = <PageHero eyebrow="Data catalog" title="Find the data, and see what depends on it." lede="Every dataset with its owner, tags, columns and freshness, and the jobs, models and services built on it. Search by name, tag, owner or column." />
  if (query.isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={5} />
      </>
    )
  }
  if (query.isError && !query.data) {
    return (
      <>
        {hero}
        <ErrorState title="Could not load the data catalog." error={query.error} onRetry={() => query.refetch()} retrying={query.isRefetching} />
      </>
    )
  }
  const items = query.data?.items ?? []
  const facets = query.data?.facets ?? { tags: {}, owners: {} }
  const open = items.find((e) => e.name === selected)

  return (
    <>
      {hero}
      <div className="toolbar" role="search">
        <label>
          Search
          <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="name, tag, owner, column…" />
        </label>
        <label>
          Tag
          <select value={tag} onChange={(e) => setTag(e.target.value)}>
            <option value="">Any</option>
            {Object.entries(facets.tags).map(([t, n]) => (
              <option key={t} value={t}>
                {t} ({n})
              </option>
            ))}
          </select>
        </label>
        <label>
          Owner
          <select value={owner} onChange={(e) => setOwner(e.target.value)}>
            <option value="">Any</option>
            {Object.entries(facets.owners).map(([o, n]) => (
              <option key={o} value={o}>
                {o} ({n})
              </option>
            ))}
          </select>
        </label>
      </div>
      {items.length === 0 ? (
        <EmptyState title={q || tag || owner ? 'No dataset matches.' : 'No datasets yet.'}>Create one on the Datasets page, then document it here.</EmptyState>
      ) : (
        <div className="table-wrap">
          <table>
            <caption className="sr-only">Datasets</caption>
            <thead>
              <tr>
                <th>Dataset</th>
                <th>Owner</th>
                <th>Tags</th>
                <th>State</th>
                <th>Updated</th>
                <th>Used by</th>
              </tr>
            </thead>
            <tbody>
              {items.map((e) => (
                <tr key={`${e.namespace}/${e.name}`}>
                  <td>
                    <button type="button" className="btn-secondary mono" aria-pressed={selected === e.name} onClick={() => setSelected(selected === e.name ? null : e.name)}>
                      {e.name}
                    </button>
                    {e.description && <div className="muted">{e.description}</div>}
                  </td>
                  <td>{e.owner || '—'}</td>
                  <td>
                    {e.tags.map((t) => (
                      <span key={t} className="pill" style={{ marginRight: '0.25rem' }}>
                        {t}
                      </span>
                    ))}
                  </td>
                  <td>
                    <span className={`pill ${phaseTone(e.state ?? undefined)}`}>{e.state ?? 'pending'}</span>
                  </td>
                  <td>{formatRelative(e.updated)}</td>
                  <td>{usedByLabel(e.usedBy)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {open && (
        <div className="grid">
          <Detail key={open.name} entry={open} onClose={() => setSelected(null)} />
        </div>
      )}
    </>
  )
}
