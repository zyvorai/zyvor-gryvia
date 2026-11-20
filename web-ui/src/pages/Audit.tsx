// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { saveBlob } from '@/lib/download'
import { NO_FILTERS, actorLabel, auditParams, outcomeTone, type AuditFilters } from '@/lib/audit'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import PageHero from '@/components/PageHero'
import { ErrorState, Skeleton } from '@/components/StateViews'

export default function Audit() {
  useDocumentTitle('Audit')
  const [draft, setDraft] = useState<AuditFilters>(NO_FILTERS)
  const [applied, setApplied] = useState<AuditFilters>(NO_FILTERS)
  const q = useQuery({ queryKey: ['audit', applied], queryFn: () => api.getAudit(auditParams(applied)) })
  const set = <K extends keyof AuditFilters>(key: K, value: AuditFilters[K]) => setDraft((d) => ({ ...d, [key]: value }))

  const hero = (
    <PageHero eyebrow="Audit" title="Who changed what, and who was refused." lede="Every state-changing request, including failed logins and refused calls. Request bodies are never recorded." />
  )
  if (q.isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={6} />
      </>
    )
  }
  if (q.isError && !q.data) {
    return (
      <>
        {hero}
        <ErrorState title="Could not load the audit trail." error={q.error} onRetry={() => q.refetch()} retrying={q.isRefetching} />
      </>
    )
  }
  const items = q.data?.items ?? []
  return (
    <>
      {hero}
      {q.data && !q.data.durable && <p className="muted">{q.data.note}</p>}
      <form
        className="toolbar"
        onSubmit={(e) => {
          e.preventDefault()
          setApplied(draft)
        }}
      >
        <label>
          Outcome
          <select value={draft.outcome} onChange={(e) => set('outcome', e.target.value as AuditFilters['outcome'])}>
            <option value="">Any</option>
            <option value="success">Success</option>
            <option value="denied">Denied</option>
            <option value="error">Error</option>
          </select>
        </label>
        <label>
          Method
          <select value={draft.method} onChange={(e) => set('method', e.target.value as AuditFilters['method'])}>
            <option value="">Any</option>
            {['POST', 'PUT', 'PATCH', 'DELETE'].map((m) => (
              <option key={m}>{m}</option>
            ))}
          </select>
        </label>
        <label>
          Actor
          <input value={draft.actor} onChange={(e) => set('actor', e.target.value)} placeholder="email or tenant" />
        </label>
        <label>
          Path contains
          <input value={draft.path} onChange={(e) => set('path', e.target.value)} placeholder="/api/models" />
        </label>
        <label>
          Since
          <input type="date" value={draft.since} onChange={(e) => set('since', e.target.value)} />
        </label>
        <button type="submit" className="btn primary">
          Apply
        </button>
        <button
          type="button"
          className="btn"
          onClick={async () => saveBlob(await api.exportAudit(auditParams(applied, 10000)), 'audit.csv')}
        >
          Export CSV
        </button>
      </form>
      {items.length === 0 ? (
        <p className="muted">No entries match.</p>
      ) : (
        <div className="table-wrap">
          <table>
            <caption className="sr-only">Audit entries, newest first</caption>
            <thead>
              <tr>
                <th>Time</th>
                <th>Actor</th>
                <th>Action</th>
                <th>Status</th>
                <th>From</th>
              </tr>
            </thead>
            <tbody>
              {items.map((e) => (
                <tr key={`${e.id}-${e.time}`}>
                  <td className="mono">{e.time.replace('T', ' ').replace(/\.\d+Z$/, 'Z')}</td>
                  <td>{actorLabel(e.actor)}</td>
                  <td>
                    <span className="mono">
                      {e.method} {e.path}
                    </span>
                  </td>
                  <td>
                    <span className={`pill ${outcomeTone(e.outcome)}`}>
                      {e.status} {e.outcome}
                    </span>
                  </td>
                  <td className="mono">{e.ip}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
