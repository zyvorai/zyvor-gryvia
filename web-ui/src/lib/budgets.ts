// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Pure helpers for the Budgets page. Amounts are estimates from usage records (see docs/gpuaas-completion.md).
export type BudgetState = 'active' | 'warning' | 'exceeded' | 'blocked'

export interface Budget {
  name: string
  scope?: { type: string; name: string }
  period?: { type?: string; startDate?: string; endDate?: string }
  limits?: { costUSD?: number; gpuHours?: number }
  enforcement?: { enabled?: boolean; action?: string } | null
  state?: BudgetState | string
  usage?: { costUSD?: number; gpuHours?: number }
  utilization?: { costPercent?: number }
  forecast?: { projectedCostUSD?: number; budgetSufficient?: boolean }
  currentPeriod?: { startDate?: string; endDate?: string; daysRemaining?: number }
}

export function budgetTone(state?: string): 'ok' | 'warn' | 'bad' {
  if (state === 'exceeded' || state === 'blocked') return 'bad'
  if (state === 'warning') return 'warn'
  return 'ok'
}

/** Cost utilization, from the server value or (older objects) recomputed from usage and limit. */
export function costPercent(b: Budget): number {
  const p = b.utilization?.costPercent
  if (typeof p === 'number' && Number.isFinite(p)) return p
  const limit = b.limits?.costUSD ?? 0
  return limit > 0 ? ((b.usage?.costUSD ?? 0) / limit) * 100 : 0
}

export function scopeLabel(b: Pick<Budget, 'scope'>): string {
  return b.scope?.name ? `${b.scope.type ?? 'scope'}/${b.scope.name}` : '—'
}

export function enforcementLabel(b: Pick<Budget, 'enforcement'>): string {
  const e = b.enforcement
  if (!e || !e.enabled) return 'alerts only'
  return e.action === 'block' ? 'hard: blocks new jobs' : e.action || 'enabled'
}

export function forecastLabel(b: Budget): string {
  const f = b.forecast
  if (!f || f.projectedCostUSD === undefined) return '—'
  return f.budgetSufficient === false ? 'will exceed' : 'within budget'
}
