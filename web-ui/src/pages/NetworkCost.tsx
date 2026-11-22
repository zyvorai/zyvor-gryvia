// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { api } from '@/lib/api'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import Progress from '@/components/Progress'
import DataTable, { type Column } from '@/components/DataTable'
import { useTableState } from '@/hooks/useTableState'
import { applySearch, type SortAccessor } from '@/lib/tableState'
import { TableCaption } from '@/components/TableCaption'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { formatBytes, formatMoney, formatPercent } from '@/lib/format'
import { errorMessage } from '@/lib/errors'
import { periodLabel, summarizeCosts, type CostGroup } from '@/lib/networkCost'

const SORTERS: Record<string, SortAccessor<CostGroup>> = {
  name: (g) => g.name.toLowerCase(),
  cost: (g) => g.cost,
  bytes: (g) => g.bytes,
}
const searchText = (g: CostGroup) => g.name

export default function NetworkCost() {
  useDocumentTitle('Network cost')
  const [params, setParams] = useSearchParams()
  const selected = params.get('period') ?? ''
  const setSelected = (p: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.set('period', p)
        next.delete('page')
        return next
      },
      { replace: true },
    )
  const { data: costData, isLoading, isError, error, refetch, isFetching, dataUpdatedAt } = useQuery({
    queryKey: ['networkCosts'],
    queryFn: api.getNetworkCosts,
    refetchInterval: 60000,
  })

  const allReports = useMemo(() => costData?.reports ?? [], [costData])
  const summary = useMemo(() => summarizeCosts(allReports, selected), [allReports, selected])

  return (
    <>
      <PageHero eyebrow="Network" title="What your traffic costs." lede="Cross-zone and egress spend per period, as reported by the network operator." />

      <div className="grid">
        {isLoading ? (
          <section className="card span3" aria-label="Live summary">
            <Skeleton rows={3} />
          </section>
        ) : isError && !costData ? (
          <div className="span3">
            <ErrorState title="Could not load network cost data." error={error} onRetry={() => refetch()} retrying={isFetching} />
          </div>
        ) : allReports.length === 0 ? (
          <div className="span3">
            <EmptyState title="No network cost reports yet">
              The network operator produces cost reports from GryviaNetworkCost resources. Create a GryviaNetworkCost for a namespace or team to start tracking; the first report appears after one reporting interval.
            </EmptyState>
          </div>
        ) : (
          <CostBody
            summary={summary}
            costPerGB={costData?.costPerGB}
            periodChoices={summary.periods}
            onSelect={setSelected}
            updatedAt={dataUpdatedAt}
            onRefresh={() => refetch()}
            refreshing={isFetching}
            stale={isError ? errorMessage(error) : undefined}
          />
        )}
      </div>
    </>
  )
}

function CostBody({ summary, costPerGB, periodChoices, onSelect, updatedAt, onRefresh, refreshing, stale }: {
  summary: ReturnType<typeof summarizeCosts>
  costPerGB?: { sameZone: number; crossZone: number; internetEgress: number }
  periodChoices: string[]
  onSelect: (p: string) => void
  updatedAt: number
  onRefresh: () => void
  refreshing: boolean
  stale?: string
}) {
  const { period, reports, totalCost, sameZone, crossZone, external, teams, namespaces, namespacesAll, trend } = summary
  const table = useTableState<CostGroup>({ rows: teams, searchText, sortAccessors: SORTERS, pageSize: 10 })
  // One search box (the teams table's) also narrows the namespaces card; unsearched it shows the top 8.
  const shownNamespaces = table.search.trim() ? applySearch(namespacesAll, table.search, searchText) : namespaces
  const teamColumns: Column<CostGroup>[] = [
    { key: 'name', header: 'Team', sortable: true, render: (t) => t.name },
    { key: 'cost', header: 'Cost (USD)', sortable: true, numeric: true, render: (t) => <span className="mono">{formatMoney(t.cost)}</span> },
    { key: 'bytes', header: 'Traffic', sortable: true, numeric: true, render: (t) => <span className="muted">{formatBytes(t.bytes)}</span> },
    { key: 'share', header: '% of total', numeric: true, render: (t) => <span className="faint">{totalCost > 0 ? formatPercent((t.cost / totalCost) * 100) : '—'}</span> },
  ]
  const totalBytes = sameZone + crossZone + external
  const label = periodLabel(period)

  return (
    <>
      <div className="toolbar span3">
        <label className="row">
          <span>Period</span>
          <select value={period} onChange={(e) => onSelect(e.target.value)} aria-label="Reporting period">
            {periodChoices.map((p, i) => (
              <option key={p} value={p}>
                {periodLabel(p)}
                {i === 0 ? ' (latest)' : ''}
              </option>
            ))}
          </select>
        </label>
        <span className="faint">
          {reports.length} report{reports.length === 1 ? '' : 's'} in this period
        </span>
        <button type="button" className="btn-secondary" onClick={onRefresh} disabled={refreshing} aria-busy={refreshing}>
          {refreshing ? 'Refreshing…' : 'Refresh'}
        </button>
      </div>

      <PagePulse
        updatedAt={updatedAt}
        error={stale}
        headline={`Network spend for ${label} is ${formatMoney(totalCost)} across ${reports.length} report${reports.length === 1 ? '' : 's'}.`}
        figures={[
          { label: `spend · ${label}`, value: formatMoney(totalCost) },
          { label: 'same-zone traffic', value: formatBytes(sameZone) },
          { label: 'cross-zone traffic', value: formatBytes(crossZone) },
          { label: 'internet egress', value: formatBytes(external) },
        ]}
      />

      <section className="card span2">
        <p className="eyebrow">ZONES · {label.toUpperCase()}</p>
        <h2 className="card-title">Traffic by zone type</h2>
        <div className="stack">
          {[
            { label: 'Same zone', bytes: sameZone, rate: costPerGB?.sameZone },
            { label: 'Cross zone', bytes: crossZone, rate: costPerGB?.crossZone },
            { label: 'Internet egress', bytes: external, rate: costPerGB?.internetEgress },
          ].map((z) => (
            <div key={z.label}>
              <div className="list-row">
                <span className="grow">{z.label}</span>
                <span className="faint num">{formatBytes(z.bytes)}</span>
                <span className="mono num">{totalBytes > 0 ? formatPercent((z.bytes / totalBytes) * 100) : '—'}</span>
              </div>
              <Progress value={z.bytes} max={totalBytes} label={`${z.label} share of traffic`} />
              {z.rate !== undefined && z.rate !== null && <small className="faint">Configured rate {formatMoney(z.rate)}/GB</small>}
            </div>
          ))}
          <small className="faint">Cost is reported per team and namespace (totals above), not per zone type.</small>
        </div>
      </section>

      <section className="card">
        <p className="eyebrow">TREND</p>
        <h2 className="card-title">Cost trend</h2>
        <CostTrendChart data={trend} />
      </section>

      <section className="card span2">
        <p className="eyebrow">TEAMS · {label.toUpperCase()}</p>
        <h2 className="card-title">Cost by team</h2>
        <DataTable
          caption={`Network cost by team, ${label}`}
          searchLabel="Search teams and namespaces"
          columns={teamColumns}
          state={table}
          rowKey={(t) => t.name}
          empty={<EmptyState title="No team costs in this period">The network operator reports cost per team in each GryviaNetworkCost report; none of this period's reports include a team breakdown.</EmptyState>}
        />
      </section>

      <section className="card">
        <p className="eyebrow">BY NAMESPACE</p>
        <h2 className="card-title">Top cost contributors</h2>
        <div className="stack">
          {shownNamespaces.length === 0 && <p className="faint">No namespace matches the search.</p>}
          {shownNamespaces.map((n) => (
            <div key={n.name}>
              <div className="list-row">
                <span className="grow">{n.name}</span>
                <span className="mono num">{formatMoney(n.cost)}</span>
              </div>
              <Progress value={n.cost} max={totalCost} label={`${n.name} share of spend`} />
            </div>
          ))}
        </div>
      </section>
    </>
  )
}

function CostTrendChart({ data }: { data: Array<{ period: string; cost: number }> }) {
  if (data.length < 2) {
    return (
      <EmptyState title="Not enough history for a trend">
        A trend needs at least two reporting periods. It appears after the next reporting interval.
      </EmptyState>
    )
  }
  const maxCost = Math.max(...data.map((d) => d.cost), 0)
  const latest = data[data.length - 1]
  const chartHeight = 120
  const barW = 24
  const gap = 4
  const width = data.length * (barW + gap) - gap
  const summary = `Network cost per period, ${data[0].period} to ${latest.period}: from ${formatMoney(data[0].cost)} to ${formatMoney(latest.cost)}, peak ${formatMoney(maxCost)}.`

  return (
    <div>
      <svg viewBox={`0 0 ${width} ${chartHeight}`} width="100%" height={chartHeight} preserveAspectRatio="none" role="img" aria-label={summary}>
        {data.map((d, i) => {
          const h = maxCost > 0 ? (d.cost / maxCost) * chartHeight : 0
          return (
            <rect key={d.period} x={i * (barW + gap)} y={chartHeight - h} width={barW} height={h} rx="3" fill="var(--apple-blue)">
              <title>{`${d.period}: ${formatMoney(d.cost)}`}</title>
            </rect>
          )
        })}
      </svg>
      <div className="list-row faint">
        <small className="grow">{data[0].period}: {formatMoney(data[0].cost)}</small>
        <small>{latest.period}: {formatMoney(latest.cost)}</small>
      </div>
      <small className="faint">Peak {formatMoney(maxCost)}. Bar height is relative to the peak.</small>
      <div className="sr-only">
        <table>
          <TableCaption>Network cost per period</TableCaption>
          <thead>
            <tr>
              <th scope="col">Period</th>
              <th scope="col" className="num">Cost (USD)</th>
            </tr>
          </thead>
          <tbody>
            {data.map((d) => (
              <tr key={d.period}>
                <td>{d.period}</td>
                <td className="num">{formatMoney(d.cost)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
