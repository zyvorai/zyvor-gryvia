// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import type { NetworkCostReport } from './api'

export type CostGroup = { name: string; cost: number; bytes: number }

export type CostSummary = {
  /** Selected period (defaults to the latest), '' when there are no reports. */
  period: string
  /** Periods present in the data, newest first. */
  periods: string[]
  /** Only the reports of `period`: periods are never summed together. */
  reports: NetworkCostReport[]
  totalCost: number
  sameZone: number
  crossZone: number
  external: number
  teams: CostGroup[]
  /** Top 8 namespaces by cost. */
  namespaces: CostGroup[]
  /** Every namespace of the period (for search). */
  namespacesAll: CostGroup[]
  /** Total per period (ISO date labels), oldest first, last 14. */
  trend: Array<{ period: string; cost: number }>
}

/** Period as an ISO date (or the raw string when it is not a date). */
export function periodLabel(period: string): string {
  return /^\d{4}-\d{2}-\d{2}/.test(period) ? period.slice(0, 10) : period || 'unknown period'
}

const key = (r: NetworkCostReport) => periodLabel(r.period ?? '')

export function summarizeCosts(all: NetworkCostReport[], selected?: string): CostSummary {
  const periods = [...new Set(all.map(key))].sort((a, b) => b.localeCompare(a))
  const period = selected && periods.includes(selected) ? selected : (periods[0] ?? '')
  const reports = all.filter((r) => key(r) === period)
  const sum = (f: (r: NetworkCostReport) => number) => reports.reduce((s, r) => s + (f(r) || 0), 0)

  const group = (nameOf: (r: NetworkCostReport) => string) => {
    const m = new Map<string, CostGroup>()
    for (const r of reports) {
      const name = nameOf(r)
      const g = m.get(name) ?? { name, cost: 0, bytes: 0 }
      g.cost += r.totalCostUSD || 0
      g.bytes += (r.sameZoneBytes || 0) + (r.crossZoneBytes || 0) + (r.externalBytes || 0)
      m.set(name, g)
    }
    return [...m.values()].sort((a, b) => b.cost - a.cost)
  }

  const allNs = group((r) => r.namespace || 'unknown')
  const perPeriod = new Map<string, number>()
  for (const r of all) perPeriod.set(key(r), (perPeriod.get(key(r)) ?? 0) + (r.totalCostUSD || 0))
  const trend = [...perPeriod.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .slice(-14)
    .map(([p, cost]) => ({ period: p, cost }))

  return {
    period,
    periods,
    reports,
    totalCost: sum((r) => r.totalCostUSD),
    sameZone: sum((r) => r.sameZoneBytes),
    crossZone: sum((r) => r.crossZoneBytes),
    external: sum((r) => r.externalBytes),
    teams: group((r) => r.team || 'unassigned'),
    namespaces: allNs.slice(0, 8),
    namespacesAll: allNs,
    trend,
  }
}
