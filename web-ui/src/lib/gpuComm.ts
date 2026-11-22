// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

export type Collectors = { reachable: number; total: number }
export type CollectorState = 'unknown' | 'none-deployed' | 'unreachable' | 'partial' | 'ok'

/** What the collector counts say about the data source (absent = older gateway = unknown). */
export function collectorState(c?: Collectors | null): CollectorState {
  if (!c || !Number.isFinite(c.total) || !Number.isFinite(c.reachable)) return 'unknown'
  if (c.total === 0) return 'none-deployed'
  if (c.reachable === 0) return 'unreachable'
  return c.reachable < c.total ? 'partial' : 'ok'
}

/** Lower-cased, trimmed identifier ("Communication " -> "communication"). */
export function normalizeBottleneck(b?: string | null): string {
  return (b ?? '').trim().toLowerCase()
}

/** Human label: underscores become spaces ("data_loading" -> "data loading"). */
export function humanize(s?: string | null): string {
  return (s ?? '').replace(/_/g, ' ').trim()
}

/** Comm/compute ratio as a percentage. Values above 1 are treated as already a percentage. */
export function ratioPercent(ratio?: number | null): number | undefined {
  if (ratio === undefined || ratio === null || !Number.isFinite(ratio) || ratio < 0) return undefined
  return ratio > 1 ? ratio : ratio * 100
}

/** "2.4x slower", or undefined when the factor is missing, non-finite or not a slowdown. */
export function slowdownLabel(factor?: number | null): string | undefined {
  if (factor === undefined || factor === null || !Number.isFinite(factor) || factor <= 0) return undefined
  return `${factor.toFixed(1)}x slower`
}

export function formatNs(ns?: number | null): string {
  if (ns === undefined || ns === null || !Number.isFinite(ns)) return '—'
  if (ns >= 1_000_000_000) return `${(ns / 1_000_000_000).toFixed(2)}s`
  if (ns >= 1_000_000) return `${(ns / 1_000_000).toFixed(2)}ms`
  if (ns >= 1_000) return `${(ns / 1_000).toFixed(1)}us`
  return `${Math.round(ns)}ns`
}
