// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// The audit trail (GET /api/audit, admin only): who changed what, and who was refused.

export interface AuditEntry {
  time: string
  id: string
  actor: { auth: string; role: string; tenant: string; subject: string }
  method: string
  route: string
  path: string
  status: number
  outcome: 'success' | 'denied' | 'error'
  ip: string
  durationMs: number
}

export interface AuditResponse {
  items: AuditEntry[]
  /** True when entries come from the audit database and survive restarts. */
  durable?: boolean
  note?: string
}

export interface AuditFilters {
  outcome: '' | AuditEntry['outcome']
  method: '' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  actor: string
  path: string
  since: string
}

export const NO_FILTERS: AuditFilters = { outcome: '', method: '', actor: '', path: '', since: '' }

/** Query parameters for the filters that are set; `since` is a date (YYYY-MM-DD) from a date input. */
export function auditParams(f: AuditFilters, limit = 200): Record<string, string | number> {
  const p: Record<string, string | number> = { limit }
  for (const key of ['outcome', 'method', 'actor', 'path'] as const) {
    const v = f[key].trim()
    if (v) p[key] = v
  }
  if (f.since) p.since = `${f.since}T00:00:00.000Z`
  return p
}

export function outcomeTone(o: AuditEntry['outcome']): 'ok' | 'warn' | 'bad' {
  return o === 'success' ? 'ok' : o === 'denied' ? 'warn' : 'bad'
}

/** "alice@x (tenant alpha)", "admin via api_key", or "anonymous". */
export function actorLabel(a: AuditEntry['actor']): string {
  if (a.subject) return a.tenant ? `${a.subject} (${a.tenant})` : a.subject
  if (a.role === 'none') return 'anonymous'
  return `${a.role} via ${a.auth}`
}
