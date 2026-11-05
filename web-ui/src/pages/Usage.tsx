import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { api } from '@/lib/api'
import { notify } from '@/lib/notify'
import { useIsAdmin } from '@/lib/useRole'
import { saveBlob } from '@/lib/download'
import { USAGE_GROUPS, defaultUsageRange, exportFilename, keyHeader, parseGroupBy, rangeError, usageParams, type UsageRow } from '@/lib/cloud'
import { formatNumber } from '@/lib/format'
import { formatRate } from '@/lib/cloud'
import type { SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import DataTable, { type Column } from '@/components/DataTable'
import PageHero from '@/components/PageHero'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

const SORTS: Record<string, SortAccessor<UsageRow>> = {
  key: (r) => r.key,
  gpuHours: (r) => r.gpuHours,
  cost: (r) => r.cost,
  jobs: (r) => r.jobs,
}
const searchText = (r: UsageRow) => r.key

export default function Usage() {
  useDocumentTitle('Usage')
  const admin = useIsAdmin()
  const [params, setParams] = useSearchParams()
  const defaults = useMemo(() => defaultUsageRange(), [])
  const from = params.get('from') ?? defaults.from
  const to = params.get('to') ?? defaults.to
  const groupBy = parseGroupBy(params.get('groupBy'), admin ? 'tenant' : 'sku')
  const tenant = admin ? (params.get('tenant') ?? '') : ''
  const kindRaw = params.get('kind') ?? 'all'
  const kind = kindRaw === 'gpu' || kindRaw === 'tokens' ? kindRaw : 'all'
  const [exporting, setExporting] = useState<'csv' | 'json' | null>(null)

  const setParam = (name: string, value: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        if (value) next.set(name, value)
        else next.delete(name)
        next.delete('page')
        return next
      },
      { replace: true },
    )

  const rangeErr = rangeError(from, to)
  const filters = { from, to, groupBy, tenant, kind }
  const query = usageParams(filters, { groupBy })

  const tenants = useQuery({ queryKey: ['tenants'], queryFn: api.getTenants, enabled: admin })
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ['usage', query],
    queryFn: () => api.getUsage(query),
    enabled: !rangeErr,
  })
  const table = useTableState({ rows: data?.items, searchText, sortAccessors: SORTS, defaultSort: { key: groupBy === 'day' ? 'key' : 'cost', dir: groupBy === 'day' ? 'asc' : 'desc' }, pageSize: 15 })

  const doExport = async (format: 'csv' | 'json') => {
    setExporting(format)
    try {
      const blob = await api.exportUsage(format, usageParams(filters))
      saveBlob(blob, exportFilename(format, filters))
    } catch (err) {
      notify.error(`Could not export ${format.toUpperCase()}`, err)
    } finally {
      setExporting(null)
    }
  }

  const currency = data?.totals.currency || 'USD'
  const cost = (n: number, c?: string) => formatRate(n, c || currency)
  const columns: Column<UsageRow>[] = [
    { key: 'key', header: keyHeader(groupBy), sortable: true, render: (r) => <span className="mono">{r.key}</span> },
    { key: 'gpuHours', header: 'GPU hours', sortable: true, numeric: true, render: (r) => formatNumber(Math.round(r.gpuHours * 100) / 100) },
    { key: 'cost', header: 'Estimated cost', sortable: true, numeric: true, render: (r) => cost(r.cost, r.currency) },
    { key: 'jobs', header: 'Jobs', sortable: true, numeric: true, render: (r) => formatNumber(r.jobs) },
  ]

  return (
    <>
      <PageHero
        eyebrow="Usage"
        title={admin ? 'Usage & FinOps.' : 'Your usage.'}
        lede="GPU hours and LLM token costs from usage records. Filter by kind to unify training and inference FinOps. Estimates, not invoices."
      />
      <div className="grid">
        <section className="card span3">
          <p className="eyebrow">Filters</p>
          <div className="toolbar">
            <label>
              From
              <input type="date" value={from} max={to || undefined} onChange={(e) => setParam('from', e.target.value)} aria-invalid={!!rangeErr} aria-describedby={rangeErr ? 'usage-range-msg' : undefined} />
            </label>
            <label>
              To
              <input type="date" value={to} min={from || undefined} onChange={(e) => setParam('to', e.target.value)} aria-invalid={!!rangeErr} />
            </label>
            <label>
              Group by
              <select value={groupBy} onChange={(e) => setParam('groupBy', e.target.value)}>
                {USAGE_GROUPS.filter((g) => admin || g.value !== 'tenant').map((g) => (
                  <option key={g.value} value={g.value}>
                    {g.label}
                  </option>
                ))}
              </select>
            </label>
            {admin && (
              <label>
                Tenant
                <select value={tenant} onChange={(e) => setParam('tenant', e.target.value)}>
                  <option value="">All tenants</option>
                  {(tenants.data ?? []).map((t) => (
                    <option key={t.metadata.name} value={t.metadata.name}>
                      {t.spec?.displayName || t.metadata.name}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <label>
              Kind
              <select value={kind} onChange={(e) => setParam('kind', e.target.value === 'all' ? '' : e.target.value)}>
                <option value="all">GPU + tokens</option>
                <option value="gpu">GPU hours</option>
                <option value="tokens">LLM tokens</option>
              </select>
            </label>
            <button type="button" className="btn-secondary" onClick={() => doExport('csv')} disabled={!!rangeErr || exporting !== null} aria-busy={exporting === 'csv'}>
              {exporting === 'csv' ? 'Exporting…' : 'Export CSV'}
            </button>
            <button type="button" className="btn-secondary" onClick={() => doExport('json')} disabled={!!rangeErr || exporting !== null} aria-busy={exporting === 'json'}>
              {exporting === 'json' ? 'Exporting…' : 'Export JSON'}
            </button>
          </div>
          {rangeErr && (
            <p id="usage-range-msg" className="warning" role="alert">
              {rangeErr}
            </p>
          )}
        </section>

        {isLoading && !rangeErr ? (
          <section className="card span3">
            <Skeleton rows={3} />
          </section>
        ) : isError ? (
          <div className="span3">
            <ErrorState title="Could not load usage." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        ) : data ? (
          <>
            <div className="card span3">
              <div className="apple-metric-band">
                <div>
                  <span>GPU hours</span>
                  <b>{formatNumber(Math.round(data.totals.gpuHours * 100) / 100)}</b>
                </div>
                <div>
                  <span>Estimated cost</span>
                  <b>{cost(data.totals.cost, data.totals.currency)}</b>
                </div>
                <div>
                  <span>Jobs</span>
                  <b>{formatNumber(data.totals.jobs)}</b>
                </div>
              </div>
              <p className="faint">Estimated from job wall-clock time; restarts, preemption and spot pricing can make real GPU time differ.</p>
            </div>
            <section className="card span3">
              <p className="eyebrow">Breakdown</p>
              <h2 className="card-title">By {keyHeader(groupBy).toLowerCase()}</h2>
              <DataTable
                caption={`Usage by ${keyHeader(groupBy).toLowerCase()}, ${from} to ${to}`}
                columns={columns}
                state={table}
                rowKey={(r) => r.key}
                searchLabel="Search usage"
                empty={<EmptyState title="No usage in this period.">Usage appears here once jobs have run in the selected date range.</EmptyState>}
              />
            </section>
          </>
        ) : null}
      </div>
    </>
  )
}
