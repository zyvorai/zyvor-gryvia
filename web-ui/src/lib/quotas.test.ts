// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest'
import type { GryviaQuota } from '@/types'
import { alertThreshold, allowedTypes, budgetPercent, budgetedCount, limitLabel, overBudgetAlert } from './quotas'

const base = (over: Partial<GryviaQuota['spec']> = {}, status?: GryviaQuota['status']): GryviaQuota => ({
  apiVersion: 'v1',
  kind: 'GryviaQuota',
  metadata: { name: 'q' },
  spec: { team: 't', namespaces: [], gpuQuota: { maxGPUs: 8, maxGPUsPerJob: 4, allowedGPUTypes: [], maxRunningJobs: 2 }, priority: 1, ...over },
  status,
})

describe('quotas', () => {
  it('defaults the alert threshold to 80', () => {
    expect(alertThreshold(base())).toBe(80)
    expect(alertThreshold(base({ budget: { monthlyBudget: 10, alertThreshold: 50, hardLimit: false } }))).toBe(50)
  })
  it('budgetPercent is undefined without a budget and 0 without status', () => {
    expect(budgetPercent(base())).toBeUndefined()
    expect(budgetPercent(base({ budget: { monthlyBudget: 100, alertThreshold: 80, hardLimit: true } }))).toBe(0)
  })
  it('flags over-threshold budgets', () => {
    const q = base({ budget: { monthlyBudget: 100, alertThreshold: 80, hardLimit: false } }, {
      phase: 'Active',
      currentUsage: { allocatedGPUs: 0, runningJobs: 0, queuedJobs: 0, gpuHours: 0 },
      budgetStatus: { spentThisMonth: 90, remainingBudget: 10, percentUsed: 90, projectedSpend: 100 },
    })
    expect(overBudgetAlert(q)).toBe(true)
    expect(overBudgetAlert(base())).toBe(false)
  })
  it('empty allowed types means All', () => {
    expect(allowedTypes(base())).toBe('All')
    expect(allowedTypes(base({ gpuQuota: { maxGPUs: 1, maxGPUsPerJob: 1, allowedGPUTypes: ['H100'], maxRunningJobs: 1 } }))).toEqual(['H100'])
  })
  it('unset limits read Unlimited', () => {
    expect(limitLabel(undefined)).toBe('Unlimited')
    expect(limitLabel(4)).toBe('4')
    expect(limitLabel(0)).toBe('0')
  })
  it('counts budgeted quotas', () => {
    expect(budgetedCount([base(), base({ budget: { monthlyBudget: 1, alertThreshold: 1, hardLimit: false } })])).toBe(1)
  })
})
