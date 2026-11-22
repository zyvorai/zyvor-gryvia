// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest'
import { periodLabel, summarizeCosts } from './networkCost'

const r = (period: string, ns: string, team: string, cost: number, same = 0, cross = 0, ext = 0) => ({ period, namespace: ns, team, totalCostUSD: cost, sameZoneBytes: same, crossZoneBytes: cross, externalBytes: ext })

describe('summarizeCosts', () => {
  const reports = [r('2026-09-01T00:00:00Z', 'a', 't1', 1, 10), r('2026-09-02T00:00:00Z', 'a', 't1', 2, 20), r('2026-09-02T00:00:00Z', 'b', '', 3, 0, 5)]
  it('defaults to the latest period and never sums periods', () => {
    const s = summarizeCosts(reports)
    expect(s.period).toBe('2026-09-02')
    expect(s.totalCost).toBe(5)
    expect(s.sameZone).toBe(20)
    expect(s.teams.map((t) => t.name)).toEqual(['unassigned', 't1'])
    expect(s.trend).toEqual([{ period: '2026-09-01', cost: 1 }, { period: '2026-09-02', cost: 5 }])
  })
  it('honours a selected period', () => {
    expect(summarizeCosts(reports, '2026-09-01').totalCost).toBe(1)
  })
  it('handles empty input', () => {
    expect(summarizeCosts([]).period).toBe('')
  })
  it('labels non-date periods as-is', () => {
    expect(periodLabel('2026-Q3')).toBe('2026-Q3')
  })
})

describe('namespacesAll', () => {
  it('keeps every namespace while namespaces stays top 8', () => {
    const rs = Array.from({ length: 10 }, (_, i) => r('2026-09-02', `ns${i}`, 't', i))
    const s = summarizeCosts(rs)
    expect(s.namespaces).toHaveLength(8)
    expect(s.namespacesAll).toHaveLength(10)
  })
})
