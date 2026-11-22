// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '@/lib/api'
import Progress from '@/components/Progress'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { TableCaption } from '@/components/TableCaption'
import { formatMoney, formatNumber } from '@/lib/format'
import { errorMessage } from '@/lib/errors'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import { SERIES_COLORS } from '@/lib/chartColors'
import { BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip, Legend, ResponsiveContainer, PieChart, Pie, Cell } from 'recharts'

const tooltipStyle = {
  background: 'var(--bg-elevated)',
  border: '1px solid var(--hairline-1)',
  borderRadius: 'var(--radius-md)',
  color: 'var(--text-primary)',
  fontSize: 12,
  boxShadow: 'var(--shadow-2)',
}
const tick = { fontSize: 11, fill: 'var(--text-tertiary)' }
const money = (value: unknown) => formatMoney(Number(value ?? 0))
const hoursLabel = (h: number) => `${formatNumber(h)} GPU-h`

export default function Costs() {
  useDocumentTitle('Costs')
  const { data: costData, isLoading, isError, error, refetch, isRefetching, dataUpdatedAt } = useQuery({
    queryKey: ['costs'],
    queryFn: api.getCostData,
    refetchInterval: 60000,
  })

  const hero = <PageHero eyebrow="Costs" title="Every dollar, tracked." lede="GPU spend by team and GPU type." />

  if (isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={4} />
      </>
    )
  }

  if (isError && !costData) {
    return (
      <>
        <PageHero eyebrow="Costs" title="Costs unavailable." tint="red" />
        <ErrorState title="Could not load cost data." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  const monthlyData = costData?.monthly || []
  const teamData = costData?.byTeam || []
  const gpuTypeData = costData?.byGPUType || []
  const hasData = teamData.length > 0 || gpuTypeData.length > 0
  // The gateway reports what the totals cover; today that is all-time spend.
  const scope = (costData as (typeof costData & { scope?: string }) | undefined)?.scope ?? 'all-time'
  const totalLabel = scope === 'all-time' ? 'Total spend to date' : 'Total spend'
  const total = costData?.totalCost ?? 0
  const gpuTotal = gpuTypeData.reduce((sum, g) => sum + g.cost, 0)

  if (!hasData) {
    return (
      <>
        {hero}
        <div className="grid">
          <section className="card span3">
            <EmptyState
              title="No cost data yet"
              action={
                <Link to="/jobs/new" className="buttonlike primary">
                  Submit a job
                </Link>
              }
            >
              The gateway computes costs from the GPU-hours of jobs, grouped by team quota (GryviaQuota). They appear here once a job has run.
            </EmptyState>
          </section>
        </div>
      </>
    )
  }

  return (
    <>
      {hero}

      <div className="grid">
        {isError && (
          <div className="span3">
            <ErrorState title="Could not refresh costs; showing the last data." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}

        <PagePulse
          updatedAt={dataUpdatedAt}
          error={isError ? errorMessage(error) : undefined}
          headline={`${formatMoney(total)} spent to date.`}
          figures={[
            { label: totalLabel, value: formatMoney(total) },
            { label: 'Teams', value: teamData.length },
            { label: 'GPU time', value: hoursLabel(gpuTypeData.reduce((sum, g) => sum + g.hours, 0)) },
          ]}
        />

        {!costData?.hasHistoricalData && monthlyData.length === 0 && (
          <p className="muted span3" role="note">
            Note: month-by-month history needs Prometheus integration, which is not configured. Totals above cover all recorded spend.
          </p>
        )}

        {monthlyData.length > 0 && (
          <section className="card span3">
            <p className="eyebrow">TREND</p>
            <h2 className="card-title">Monthly cost trend</h2>
            <ResponsiveContainer width="100%" height={300}>
              <BarChart data={monthlyData}>
                <CartesianGrid strokeDasharray="3 3" stroke="var(--hairline-1)" />
                <XAxis dataKey="month" tick={tick} stroke="var(--hairline-1)" />
                <YAxis tick={tick} stroke="var(--hairline-1)" tickFormatter={money} />
                <Tooltip contentStyle={tooltipStyle} formatter={money} />
                <Legend wrapperStyle={{ fontSize: 12, color: 'var(--text-secondary)' }} />
                <Bar dataKey="cost" fill="var(--apple-blue)" name="Cost (USD)" radius={[4, 4, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
            <table className="sr-only">
              <TableCaption>Monthly cost</TableCaption>
              <thead>
                <tr>
                  <th scope="col">Month</th>
                  <th scope="col">Cost</th>
                </tr>
              </thead>
              <tbody>
                {monthlyData.map((m) => (
                  <tr key={m.month}>
                    <td>{m.month}</td>
                    <td>{formatMoney(m.cost)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        )}

        {teamData.length > 0 && (
          <section className="card span2">
            <p className="eyebrow">TEAMS</p>
            <h2 className="card-title">Cost by team</h2>
            <ResponsiveContainer width="100%" height={300}>
              <PieChart>
                <Pie data={teamData} dataKey="cost" nameKey="team" cx="50%" cy="50%" outerRadius={100} label={false} labelLine={false}>
                  {teamData.map((_entry, index) => (
                    <Cell key={`cell-${index}`} fill={SERIES_COLORS[index % SERIES_COLORS.length]} />
                  ))}
                </Pie>
                <Tooltip contentStyle={tooltipStyle} formatter={money} />
                <Legend wrapperStyle={{ fontSize: 12, color: 'var(--text-secondary)' }} />
              </PieChart>
            </ResponsiveContainer>
            {teamData.map((t) => (
              <div key={t.team} className="list-row">
                <span className="grow mono">{t.team}</span>
                <span className="num">{formatMoney(t.cost)}</span>
              </div>
            ))}
          </section>
        )}

        {gpuTypeData.length > 0 && (
          <section className="card">
            <p className="eyebrow">GPU TYPES</p>
            <h2 className="card-title">Cost by GPU type</h2>
            <div className="stack">
              {gpuTypeData.map((gpu) => {
                const percentage = gpuTotal > 0 ? (gpu.cost / gpuTotal) * 100 : 0
                return (
                  <div key={gpu.type}>
                    <div className="row">
                      <span>{gpu.type}</span>
                      <span className="num">
                        {formatMoney(gpu.cost)} <span className="faint">({hoursLabel(gpu.hours)})</span>
                      </span>
                    </div>
                    <Progress value={percentage} label={`${gpu.type} share of spend`} />
                  </div>
                )
              })}
            </div>

            <p className="eyebrow">GPU PRICING</p>
            {gpuTypeData.map((gpu) => (
              <div key={gpu.type} className="list-row">
                <span className="grow">{gpu.type}</span>
                <span className="faint num">{hoursLabel(gpu.hours)}</span>
                <span className="num">{gpu.hours > 0 ? `${formatMoney(gpu.cost / gpu.hours)}/hr` : '—'}</span>
              </div>
            ))}
          </section>
        )}
      </div>
    </>
  )
}
