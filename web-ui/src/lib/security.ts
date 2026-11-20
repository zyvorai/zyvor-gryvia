// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import type { CreateSecurityPolicyRequest, SecurityPolicy } from './api'
import { validateK8sName } from './network'

export const RULE_CATALOG: Array<{ type: string; name: string; description: string }> = [
  { type: 'escape', name: 'Container escape', description: 'Detects processes breaking out of their container namespace.' },
  { type: 'mining', name: 'Cryptomining', description: 'Flags known miner binaries and mining-pool connections.' },
  { type: 'exfiltration', name: 'Data exfiltration', description: 'Flags unusual outbound transfers of sensitive data.' },
  { type: 'privesc', name: 'Privilege escalation', description: 'Detects setuid, capability and credential escalation.' },
  { type: 'driver_fim', name: 'Driver file integrity', description: 'Watches GPU driver and kernel module files for changes.' },
]

/** Friendly rule name; unknown types are humanised (underscores replaced, sentence case). */
export function ruleName(type: string): string {
  const known = RULE_CATALOG.find((r) => r.type === type)
  if (known) return known.name
  const t = type.replace(/_/g, ' ').trim()
  return t ? t.charAt(0).toUpperCase() + t.slice(1) : 'Unknown'
}

const isActive = (p: SecurityPolicy) => (p.status?.phase ?? '').toLowerCase() === 'active'

/** Rule types with at least one enabled rule in an Active policy (Pending/Failed policies protect nothing). */
export function activeRuleTypes(policies: SecurityPolicy[] | undefined): Set<string> {
  const out = new Set<string>()
  for (const p of policies ?? []) {
    if (!isActive(p)) continue
    for (const r of p.spec.detectionRules ?? []) if (r.enabled) out.add(r.type)
  }
  return out
}

export function countActivePolicies(policies: SecurityPolicy[] | undefined): number {
  return (policies ?? []).filter(isActive).length
}

/** Real per-policy counters, aggregated. */
export function policyCounters(policies: SecurityPolicy[] | undefined): { alerts: number; lastAlert?: string; byType: Array<[string, number]> } {
  let alerts = 0
  let last: string | undefined
  const by = new Map<string, number>()
  for (const p of policies ?? []) {
    alerts += p.status?.alertsTriggered ?? 0
    const la = p.status?.lastAlert
    if (la && (!last || new Date(la).getTime() > new Date(last).getTime())) last = la
    for (const [k, v] of Object.entries(p.status?.detectionCounts ?? {})) by.set(k, (by.get(k) ?? 0) + v)
  }
  return { alerts, lastAlert: last, byType: [...by.entries()].sort((a, b) => b[1] - a[1]) }
}

// ---- create form -------------------------------------------------------------------------------

export const SENSITIVITIES = ['low', 'medium', 'high'] as const
export const MAX_NAMESPACES = 50

export type RuleDraft = { type: string; enabled: boolean; sensitivity: string }
export type SecurityFormValues = { name: string; namespaces: string[]; rules: RuleDraft[]; webhook: string; autoBlock: boolean }
export type SecurityFormErrors = { name?: string; namespaces?: string; rules?: string; webhook?: string }

/** One draft row per known rule type: all disabled, medium sensitivity. */
export function defaultRuleDrafts(): RuleDraft[] {
  return RULE_CATALOG.map((r) => ({ type: r.type, enabled: false, sensitivity: 'medium' }))
}

/** Error for a namespace about to be added as a chip (undefined when fine). */
export function validateNamespaceChip(value: string, existing: string[]): string | undefined {
  const v = value.trim()
  const bad = validateK8sName(v, 'Namespace')
  if (bad) return bad
  if (existing.includes(v)) return `${v} is already in the list.`
  if (existing.length >= MAX_NAMESPACES) return `At most ${MAX_NAMESPACES} namespaces.`
  return undefined
}

/** Empty is fine (optional); otherwise an http(s) URL with a host, up to 2048 chars, as the gateway requires. */
export function validateWebhook(raw: string): string | undefined {
  const v = raw.trim()
  if (!v) return undefined
  if (v.length > 2048) return 'Webhook URL must be 2048 characters or fewer.'
  try {
    const u = new URL(v)
    if ((u.protocol !== 'http:' && u.protocol !== 'https:') || !u.hostname) return 'Webhook must be an http(s) URL.'
  } catch {
    return 'Webhook must be an http(s) URL, e.g. https://hooks.example.com/alerts.'
  }
  return undefined
}

export function validateSecurityForm(f: SecurityFormValues): SecurityFormErrors {
  const e: SecurityFormErrors = {}
  const name = validateK8sName(f.name.trim(), 'Name')
  if (name) e.name = name
  if (f.namespaces.length === 0) e.namespaces = 'Add at least one target namespace.'
  if (!f.rules.some((r) => r.enabled)) e.rules = 'Enable at least one detection rule.'
  const w = validateWebhook(f.webhook)
  if (w) e.webhook = w
  return e
}

export function buildSecurityRequest(f: SecurityFormValues): CreateSecurityPolicyRequest {
  const webhook = f.webhook.trim()
  return {
    name: f.name.trim(),
    targetNamespaces: f.namespaces,
    detectionRules: f.rules.map((r) => ({ type: r.type, enabled: r.enabled, sensitivity: r.sensitivity })),
    autoBlock: f.autoBlock,
    ...(webhook ? { alertWebhook: webhook } : {}),
  }
}
