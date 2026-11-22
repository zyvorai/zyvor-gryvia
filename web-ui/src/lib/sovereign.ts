// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// The Sovereign AI OS overview (GET /api/sovereign): Zyntra and Netra next to Gryvia, probed by the gateway.

export interface SovereignProduct {
  name: 'Zyntra' | 'Netra'
  role: string
  installed: boolean
  /** "healthy", "not installed", "unreachable" or "unhealthy (<code>)". */
  state: string
  console?: string | null
  error?: string
  version?: string
  executeMode?: string
  pack?: string
  sources?: { total: number; healthy: number }
  openGaps?: number
  pendingProposals?: number
}

export interface SovereignStatus {
  enabled: boolean
  products: SovereignProduct[]
}

export function stateTone(state: string): 'ok' | 'warn' | 'bad' | '' {
  if (state === 'healthy') return 'ok'
  if (state === 'not installed') return ''
  if (state.startsWith('unhealthy')) return 'warn'
  return 'bad'
}

export function product(s: SovereignStatus | undefined, name: SovereignProduct['name']): SovereignProduct | undefined {
  return s?.products.find((p) => p.name === name)
}

function base(url: string): string {
  return url.replace(/\/+$/, '')
}

/** Zyntra's console uses hash routes. */
export function zyntraLink(url: string | null | undefined, page: 'approvals' | 'gaps' | 'audit' | 'overview'): string | undefined {
  return url ? `${base(url)}/#/${page}` : undefined
}

export function zyntraDecisionLink(url: string | null | undefined, proposalId: string): string | undefined {
  return url ? `${base(url)}/#/decision/${encodeURIComponent(proposalId)}` : undefined
}

/** The first Zyntra proposal id (prop-<hex>) an agent's answer mentions. */
export function proposalIdIn(text: string): string | undefined {
  return /\bprop-[0-9a-f]+\b/.exec(text)?.[0]
}

/** "v0.4.0 · applies approved changes · 6/6 sources healthy". */
export function zyntraSummary(p: SovereignProduct): string {
  const parts: string[] = []
  if (p.version) parts.push(`v${p.version.replace(/^v/, '')}`)
  if (p.executeMode) parts.push(p.executeMode === 'apply' ? 'applies approved changes' : `approved changes: ${p.executeMode}`)
  if (p.sources) parts.push(`${p.sources.healthy}/${p.sources.total} sources healthy`)
  return parts.join(' · ')
}
