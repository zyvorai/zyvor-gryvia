// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { budgetTone, costPercent, enforcementLabel, forecastLabel, scopeLabel } from './budgets'

describe('budgets helpers', () => {
  it('maps states to tones', () => {
    expect(budgetTone('active')).toBe('ok')
    expect(budgetTone('warning')).toBe('warn')
    expect(budgetTone('exceeded')).toBe('bad')
    expect(budgetTone('blocked')).toBe('bad')
    expect(budgetTone(undefined)).toBe('ok')
  })
  it('prefers the server utilization, else computes from usage', () => {
    expect(costPercent({ name: 'a', utilization: { costPercent: 42 } })).toBe(42)
    expect(costPercent({ name: 'a', usage: { costUSD: 25 }, limits: { costUSD: 100 } })).toBe(25)
    expect(costPercent({ name: 'a', usage: { costUSD: 25 } })).toBe(0)
  })
  it('labels scope, enforcement and forecast', () => {
    expect(scopeLabel({ scope: { type: 'team', name: 'ml' } })).toBe('team/ml')
    expect(scopeLabel({})).toBe('—')
    expect(enforcementLabel({ enforcement: null })).toBe('alerts only')
    expect(enforcementLabel({ enforcement: { enabled: true, action: 'block' } })).toBe('hard: blocks new jobs')
    expect(forecastLabel({ name: 'a' })).toBe('—')
    expect(forecastLabel({ name: 'a', forecast: { projectedCostUSD: 5, budgetSufficient: false } })).toBe('will exceed')
    expect(forecastLabel({ name: 'a', forecast: { projectedCostUSD: 5, budgetSufficient: true } })).toBe('within budget')
  })
})
