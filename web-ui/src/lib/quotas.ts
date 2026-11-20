// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import type { GryviaQuota } from '@/types'

export const DEFAULT_ALERT_THRESHOLD = 80

/** Alert threshold in percent; the CRD field is optional. */
export function alertThreshold(q: GryviaQuota): number {
  return q.spec.budget?.alertThreshold ?? DEFAULT_ALERT_THRESHOLD
}

/** Percent of the monthly budget used, or undefined when the quota has no budget. */
export function budgetPercent(q: GryviaQuota): number | undefined {
  const budget = q.spec.budget
  if (!budget) return undefined
  const spent = q.status?.budgetStatus?.spentThisMonth ?? 0
  const pct = q.status?.budgetStatus?.percentUsed ?? (budget.monthlyBudget > 0 ? (spent / budget.monthlyBudget) * 100 : 0)
  return Number.isFinite(pct) ? pct : 0
}

/** True when a budgeted quota has crossed its alert threshold. */
export function overBudgetAlert(q: GryviaQuota): boolean {
  const pct = budgetPercent(q)
  return pct !== undefined && pct > alertThreshold(q)
}

/** Allowed GPU types for display; an empty or missing list means every type is allowed. */
export function allowedTypes(q: GryviaQuota): string[] | 'All' {
  const t = q.spec.gpuQuota?.allowedGPUTypes
  return t && t.length > 0 ? t : 'All'
}

/** A limit that may be unset (undefined/null/0 is not a limit in the CRD, so unset only) . */
export function limitLabel(n: number | undefined | null): string {
  return n === undefined || n === null ? 'Unlimited' : String(n)
}

/** How many of the quotas have a budget configured. */
export function budgetedCount(list: GryviaQuota[]): number {
  return list.filter((q) => q.spec.budget).length
}
