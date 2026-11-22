// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { budgetTone, costPercent, enforcementLabel, forecastLabel, scopeLabel, type Budget } from '@/lib/budgets'
import { formatMoney, formatNumber, formatPercent } from '@/lib/format'
import type { SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import DataTable, { type Column } from '@/components/DataTable'
import PageHero from '@/components/PageHero'
import Progress from '@/components/Progress'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

const SORTS: Record<string, SortAccessor<Budget>> = {
  name: (b) => b.name,
  scope: (b) => scopeLabel(b),
  used: (b) => b.usage?.costUSD ?? 0,
  percent: (b) => costPercent(b),
  state: (b) => b.state ?? '',
}
const searchText = (b: Budget) => `${b.name} ${scopeLabel(b)} ${b.state ?? ''}`

export default function Budgets() {
  useDocumentTitle('Budgets')
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({ queryKey: ['budgets'], queryFn: api.getBudgets, refetchInterval: 60000 })
  const table = useTableState({ rows: data, searchText, sortAccessors: SORTS, defaultSort: { key: 'name', dir: 'asc' }, pageSize: 12 })

  const hero = <PageHero eyebrow="Budgets" title="Budgets." lede="Estimated spend from usage records against each limit. Spend is metered wall-clock time times the SKU rate, not an invoice." />
  if (isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={4} />
      </>
    )
  }
  if (isError && !data) {
    return (
      <>
        <PageHero eyebrow="Budgets" title="Budgets unavailable." tint="red" />
        <ErrorState title="Could not load budgets." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  const columns: Column<Budget>[] = [
    { key: 'name', header: 'Budget', sortable: true, render: (b) => <span className="mono">{b.name}</span> },
    { key: 'scope', header: 'Scope', sortable: true, render: (b) => scopeLabel(b) },
    { key: 'used', header: 'Spent / limit', sortable: true, numeric: true, render: (b) => `${formatMoney(b.usage?.costUSD ?? 0)} / ${b.limits?.costUSD ? formatMoney(b.limits.costUSD) : '—'}` },
    {
      key: 'percent',
      header: 'Utilization',
      sortable: true,
      render: (b) => (
        <div>
          <Progress value={costPercent(b)} label={`${b.name} cost utilization`} tone={budgetTone(b.state)} />
          <span className="faint">{formatPercent(costPercent(b))}</span>
        </div>
      ),
    },
    { key: 'gpuHours', header: 'GPU hours', numeric: true, render: (b) => formatNumber(b.usage?.gpuHours ?? 0) },
    { key: 'state', header: 'State', sortable: true, render: (b) => <span className={`pill ${budgetTone(b.state)}`}>{b.state ?? 'unknown'}</span> },
    {
      key: 'forecast',
      header: 'Forecast',
      render: (b) => (
        <span title={b.forecast?.projectedCostUSD !== undefined ? `Projected ${formatMoney(b.forecast.projectedCostUSD)} by period end` : undefined}>{forecastLabel(b)}</span>
      ),
    },
    { key: 'enforcement', header: 'Enforcement', render: (b) => enforcementLabel(b) },
  ]

  return (
    <>
      {hero}
      <div className="grid">
        {isError && (
          <div className="span3">
            <ErrorState title="Could not refresh budgets; showing the last data." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}
        <section className="card span3">
          <p className="eyebrow">Budgets</p>
          <h2 className="card-title">Spend against limits</h2>
          <DataTable caption="Budgets and spend" columns={columns} state={table} rowKey={(b) => b.name} searchLabel="Search budgets" empty={<EmptyState title="No budgets.">Budgets are defined by the platform admin.</EmptyState>} />
        </section>
      </div>
    </>
  )
}
