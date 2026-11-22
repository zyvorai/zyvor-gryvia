// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { Fragment, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { api } from '@/lib/api'
import type { NetworkFlow } from '@/lib/api'
import PageHero from '@/components/PageHero'
import { TableCaption } from '@/components/TableCaption'
import PagePulse from '@/components/kit/PagePulse'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { formatBytes, formatDate, formatRelative } from '@/lib/format'
import { errorMessage } from '@/lib/errors'
import { applySort, type SortAccessor, type SortDir } from '@/lib/tableState'
import { filterFlows, flowFilterOptions, pageWindow, parseByteQuantity, parseLatencyMs, policyPrefillUrl, traceUrl, verdictKind, verdictTone, type FlowFilters } from '@/lib/network'

const ITEMS_PER_PAGE = 20

const SORT_ACCESSORS: Record<string, SortAccessor<NetworkFlow>> = {
  service: (f) => f.spec.source.toLowerCase(),
  destination: (f) => f.spec.destination.toLowerCase(),
  verdict: (f) => (f.spec.verdict || '').toLowerCase() || undefined,
  latency: (f) => parseLatencyMs(f.spec.latency),
  throughput: (f) => parseByteQuantity(f.spec.bytes),
}

export default function NetworkFlows() {
  useDocumentTitle('Network flows')
  const [params, setParams] = useSearchParams()
  const filters: FlowFilters = {
    service: params.get('service') ?? '',
    namespace: params.get('namespace') ?? '',
    verdict: params.get('verdict') ?? '',
    protocol: params.get('protocol') ?? '',
    source: params.get('source') ?? '',
    destination: params.get('destination') ?? '',
  }
  const sortParam = params.get('sort') ?? ''
  const sortKey = sortParam in SORT_ACCESSORS ? sortParam : undefined
  const sortDir: SortDir = params.get('dir') === 'desc' ? 'desc' : 'asc'
  const toggleSort = (key: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.set('sort', key)
        next.set('dir', sortKey === key && sortDir === 'asc' ? 'desc' : 'asc')
        next.delete('page')
        return next
      },
      { replace: true },
    )
  const [expanded, setExpanded] = useState<string | null>(null)
  const requestedPage = Math.max(1, parseInt(params.get('page') ?? '1', 10) || 1)

  const setFilter = (key: keyof FlowFilters, value: string) => {
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        if (value) next.set(key, value)
        else next.delete(key)
        next.delete('page')
        return next
      },
      { replace: true },
    )
  }
  const setPage = (n: number) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        if (n > 1) next.set('page', String(n))
        else next.delete('page')
        return next
      },
      { replace: true },
    )
  const clearFilters = () => setParams({}, { replace: true })

  const { data: flows, isLoading, isError, error, refetch, isFetching, dataUpdatedAt } = useQuery({
    queryKey: ['networkFlows'],
    queryFn: api.getNetworkFlows,
    refetchInterval: 5000,
  })

  const all = useMemo(() => flows ?? [], [flows])
  const options = useMemo(() => flowFilterOptions(all), [all])
  const filtered = useMemo(
    () => applySort(filterFlows(all, filters), sortKey, sortDir, SORT_ACCESSORS),
    [all, filters.service, filters.namespace, filters.verdict, filters.protocol, filters.source, filters.destination, sortKey, sortDir], // eslint-disable-line react-hooks/exhaustive-deps
  )

  const totalPages = Math.max(1, Math.ceil(filtered.length / ITEMS_PER_PAGE))
  const page = Math.min(requestedPage, totalPages)
  const paginated = filtered.slice((page - 1) * ITEMS_PER_PAGE, page * ITEMS_PER_PAGE)
  const hasThroughput = filtered.some((f) => f.spec.bytes !== undefined && f.spec.bytes !== null && String(f.spec.bytes).trim() !== '')
  const filtersActive = Object.values(filters).some(Boolean)

  const withCurrent = (list: string[], current: string) => (current && !list.some((v) => v.toLowerCase() === current.toLowerCase()) ? [...list, current] : list)

  const dropped = filtered.filter((f) => verdictKind(f.spec.verdict) === 'drop').length
  const errored = filtered.filter((f) => verdictKind(f.spec.verdict) === 'error').length
  const forwarded = filtered.filter((f) => verdictKind(f.spec.verdict) === 'good').length
  const problems = dropped + errored
  const loaded = flows !== undefined

  return (
    <>
      <PageHero eyebrow="Network" title="Observed connections." lede="Connections observed between services, as reported by the service graph." />

      <div className="grid">
        <div className="toolbar span3">
          <Link to="/network" className="buttonlike btn-secondary">
            Back to network
          </Link>
          <button type="button" className="btn-secondary" onClick={() => refetch()} disabled={isFetching} aria-busy={isFetching}>
            {isFetching ? 'Refreshing…' : 'Refresh'}
          </button>
        </div>

        {isLoading ? (
          <section className="card span3" aria-label="Live summary">
            <Skeleton rows={2} />
          </section>
        ) : isError && !loaded ? (
          <div className="span3">
            <ErrorState title="Could not load network flows." error={error} onRetry={() => refetch()} retrying={isFetching} />
          </div>
        ) : (
          <>
            <PagePulse
              updatedAt={dataUpdatedAt}
              error={isError ? errorMessage(error) : undefined}
              headline={
                problems > 0
                  ? `${problems} connection${problems === 1 ? '' : 's'} dropped, denied or errored.`
                  : 'No dropped, denied or errored connections in view.'
              }
              tone={problems > 0 ? 'warn' : undefined}
              figures={[
                { label: 'connections shown', value: filtered.length },
                { label: 'forwarded', value: forwarded },
                { label: 'dropped / denied / error', value: problems, tone: problems > 0 ? 'warn' : undefined },
                { label: 'total observed', value: all.length },
              ]}
            />

            <section className="card span3">
              <p className="eyebrow">FILTERS</p>
              <h2 className="card-title">Narrow the view</h2>
              <div className="filters">
                <label>
                  Service
                  <input type="text" value={filters.service} onChange={(e) => setFilter('service', e.target.value)} placeholder="Filter by service..." />
                </label>
                {(filters.source || filters.destination) && (
                  <>
                    <label>
                      Source (exact)
                      <input type="text" value={filters.source} onChange={(e) => setFilter('source', e.target.value)} />
                    </label>
                    <label>
                      Destination (exact)
                      <input type="text" value={filters.destination} onChange={(e) => setFilter('destination', e.target.value)} />
                    </label>
                  </>
                )}
                <label>
                  Namespace
                  <select value={filters.namespace} onChange={(e) => setFilter('namespace', e.target.value)}>
                    <option value="">All</option>
                    {withCurrent(options.namespaces, filters.namespace).map((v) => (
                      <option key={v} value={v}>
                        {v}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  Verdict
                  <select value={filters.verdict} onChange={(e) => setFilter('verdict', e.target.value)}>
                    <option value="">All</option>
                    {withCurrent(options.verdicts, filters.verdict).map((v) => (
                      <option key={v} value={v}>
                        {v}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  Protocol
                  <select value={filters.protocol} onChange={(e) => setFilter('protocol', e.target.value)}>
                    <option value="">All</option>
                    {withCurrent(options.protocols, filters.protocol).map((v) => (
                      <option key={v} value={v}>
                        {v}
                      </option>
                    ))}
                  </select>
                </label>
                {filtersActive && (
                  <button type="button" className="btn-secondary" onClick={clearFilters}>
                    Clear filters
                  </button>
                )}
              </div>
            </section>

            <section className="card span3">
              <p className="eyebrow">
                RESULTS · PAGE {page} OF {totalPages}
              </p>
              <h2 className="card-title">
                {filtered.length} connection{filtered.length === 1 ? '' : 's'}
              </h2>
              {all.length === 0 ? (
                <EmptyState title="No connections observed yet" action={<Link to="/network" className="buttonlike btn-secondary">Back to network</Link>}>
                  Connections come from the GryviaServiceGraph that the network-intelligence operator builds from eBPF flow data. The operator has not reported any yet.
                </EmptyState>
              ) : filtered.length === 0 ? (
                <EmptyState title="No connections match these filters" action={<button type="button" className="btn-secondary" onClick={clearFilters}>Clear filters</button>}>
                  {all.length} connection{all.length === 1 ? ' is' : 's are'} hidden by the current filters.
                </EmptyState>
              ) : (
                <div className="table-wrap">
                  <table>
                    <TableCaption>{`Observed connections, page ${page} of ${totalPages}`}</TableCaption>
                    <thead>
                      <tr>
                        <th scope="col">Graph updated</th>
                        <SortTh label="Service" k="service" sortKey={sortKey} sortDir={sortDir} onSort={toggleSort} />
                        <SortTh label="Destination" k="destination" sortKey={sortKey} sortDir={sortDir} onSort={toggleSort} />
                        <th scope="col">Protocol</th>
                        <th scope="col" className="num">Port</th>
                        {hasThroughput && <SortTh label="Throughput" k="throughput" num sortKey={sortKey} sortDir={sortDir} onSort={toggleSort} />}
                        <SortTh label="Latency" k="latency" num sortKey={sortKey} sortDir={sortDir} onSort={toggleSort} />
                        <SortTh label="Verdict" k="verdict" sortKey={sortKey} sortDir={sortDir} onSort={toggleSort} />
                      </tr>
                    </thead>
                    <tbody>
                      {paginated.map((flow) => (
                        <FlowRow
                          key={flow.metadata.name}
                          flow={flow}
                          showThroughput={hasThroughput}
                          colSpan={hasThroughput ? 8 : 7}
                          open={expanded === flow.metadata.name}
                          onToggle={() => setExpanded(expanded === flow.metadata.name ? null : flow.metadata.name)}
                        />
                      ))}
                    </tbody>
                  </table>
                </div>
              )}

              {totalPages > 1 && (
                <nav aria-label="Pagination" className="toolbar">
                  <button type="button" className="btn-secondary" onClick={() => setPage(page - 1)} disabled={page === 1}>
                    Previous
                  </button>
                  {pageWindow(page, totalPages).map((n) => (
                    <button
                      key={n}
                      type="button"
                      className={n === page ? 'primary' : 'btn-secondary'}
                      aria-label={`Page ${n}`}
                      aria-current={n === page ? 'page' : undefined}
                      onClick={() => setPage(n)}
                    >
                      {n}
                    </button>
                  ))}
                  <button type="button" className="btn-secondary" onClick={() => setPage(page + 1)} disabled={page === totalPages}>
                    Next
                  </button>
                </nav>
              )}
            </section>
          </>
        )}
      </div>
    </>
  )
}

function SortTh({ label, k, num, sortKey, sortDir, onSort }: { label: string; k: string; num?: boolean; sortKey?: string; sortDir: SortDir; onSort: (k: string) => void }) {
  const sorted = sortKey === k
  return (
    <th scope="col" className={num ? 'num' : undefined} aria-sort={sorted ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none'}>
      <button type="button" className="th-sort" onClick={() => onSort(k)}>
        {label}
        <span aria-hidden="true">{sorted ? (sortDir === 'asc' ? ' ▲' : ' ▼') : ''}</span>
      </button>
    </th>
  )
}

function FlowRow({ flow, showThroughput, colSpan, open, onToggle }: { flow: NetworkFlow; showThroughput: boolean; colSpan: number; open: boolean; onToggle: () => void }) {
  const navigate = useNavigate()
  const ts = flow.spec.timestamp
  const verdict = flow.spec.verdict as string
  const latency = flow.spec.latency && flow.spec.latency !== '-' ? flow.spec.latency : '—'
  const bytes = parseByteQuantity(flow.spec.bytes)
  const throughput = bytes !== undefined ? formatBytes(bytes) : flow.spec.bytes || '—'
  const detailId = `flow-detail-${flow.metadata.name}`
  const detail: Array<[string, string]> = [
    ['Source', flow.spec.source],
    ['Destination', flow.spec.destination],
    ['Protocol', flow.spec.protocol || '—'],
    ['Port', String(flow.spec.port ?? '—')],
    ['Verdict', verdict || 'Unknown'],
    ['Latency', latency],
    ['Throughput', throughput],
    ['Graph updated', ts ? `${formatDate(ts)} · ${formatRelative(ts)}` : '—'],
  ]
  return (
    <Fragment>
      <tr
        data-clickable=""
        tabIndex={0}
        aria-expanded={open}
        aria-controls={open ? detailId : undefined}
        aria-label={`${flow.spec.source} to ${flow.spec.destination}, ${verdict || 'unknown verdict'}. ${open ? 'Hide' : 'Show'} details.`}
        onClick={onToggle}
        onKeyDown={(e) => {
          if ((e.key === 'Enter' || e.key === ' ') && e.target === e.currentTarget) {
            e.preventDefault()
            onToggle()
          }
        }}
      >
        <td className="faint" title={ts ? formatDate(ts) : undefined}>
          {ts ? `${formatDate(ts)} · ${formatRelative(ts)}` : '—'}
        </td>
        <td>{flow.spec.source}</td>
        <td>{flow.spec.destination}</td>
        <td>
          <span className="pill info">{flow.spec.protocol}</span>
        </td>
        <td className="mono muted num">{flow.spec.port}</td>
        {showThroughput && <td className="muted num">{throughput}</td>}
        <td className="muted num">{latency}</td>
        <td>
          <span className={`pill ${verdictTone(verdict)}`}>{verdict || 'Unknown'}</span>
        </td>
      </tr>
      {open && (
        <tr id={detailId}>
          <td colSpan={colSpan}>
            <div className="stack">
              <dl className="formgrid">
                {detail.map(([k, v]) => (
                  <div key={k}>
                    <dt className="faint">{k}</dt>
                    <dd className="mono">{v}</dd>
                  </div>
                ))}
              </dl>
              <div className="toolbar">
                <button
                  type="button"
                  className="primary"
                  onClick={() => navigate(policyPrefillUrl({ source: flow.spec.source, destination: flow.spec.destination, port: flow.spec.port, protocol: flow.spec.protocol }))}
                >
                  Create policy from this connection
                </button>
                <button type="button" className="btn-secondary" onClick={() => navigate(traceUrl(flow.spec.destination))}>
                  Start trace
                </button>
              </div>
            </div>
          </td>
        </tr>
      )}
    </Fragment>
  )
}
