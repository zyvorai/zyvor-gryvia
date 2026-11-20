// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Metric formatting and parameter-space validation for the auto tuner.

const RATIO_METRIC = /accuracy|precision|recall|f1|\bacc\b/i

/** Accuracy-like metrics are reported as 0-1 fractions; only those get a percent sign. */
export function looksLikeRatioMetric(metricName?: string): boolean {
  return !!metricName && RATIO_METRIC.test(metricName)
}

/** Up to 4 significant digits (large values become whole numbers with separators). */
export function formatNumber(v: number): string {
  if (!Number.isFinite(v)) return '—'
  if (Math.abs(v) >= 10000) return Math.round(v).toLocaleString('en-US')
  return String(Number(v.toPrecision(4)))
}

/** Per-objective metric text: 0.9234 accuracy -> "92.34%", 0.0312 loss -> "0.0312". */
export function formatMetric(value: number | null | undefined, metricName?: string): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—'
  if (looksLikeRatioMetric(metricName) && Math.abs(value) <= 1) return `${formatNumber(value * 100)}%`
  return formatNumber(value)
}

export type Direction = 'maximize' | 'minimize'

type TunerLike = { spec?: { direction?: string; metricName?: string; objectiveMetric?: string }; status?: { bestMetricValue?: number } }

export function metricNameOf(t: TunerLike): string | undefined {
  return t.spec?.metricName || t.spec?.objectiveMetric || undefined
}

/**
 * Best value across tuners. Only meaningful when they optimise the same metric in the same
 * direction; otherwise `mixed` is true and no value is returned. Handles negative and minimised values.
 */
export function bestAcross(tuners: TunerLike[] | undefined): { metricName?: string; value?: number; mixed: boolean } {
  const withBest = (tuners ?? []).filter((t) => typeof t.status?.bestMetricValue === 'number' && Number.isFinite(t.status.bestMetricValue))
  if (withBest.length === 0) return { mixed: false }
  const names = new Set(withBest.map((t) => metricNameOf(t) ?? ''))
  const dirs = new Set(withBest.map((t) => t.spec?.direction ?? 'maximize'))
  if (names.size > 1 || dirs.size > 1) return { mixed: true }
  const minimize = dirs.has('minimize')
  let best = withBest[0].status!.bestMetricValue as number
  for (const t of withBest) {
    const v = t.status!.bestMetricValue as number
    if (minimize ? v < best : v > best) best = v
  }
  return { metricName: metricNameOf(withBest[0]), value: best, mixed: false }
}

/** Live validation of the parameter-space JSON, mirroring the gateway (null when valid). */
export function validateParameterSpace(text: string): string | null {
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch (e) {
    return `Not valid JSON: ${e instanceof Error ? e.message : 'parse error'}`
  }
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw) || Object.keys(raw).length === 0) {
    return 'Parameter space must be a non-empty JSON object.'
  }
  const entries = Object.entries(raw as Record<string, unknown>)
  if (entries.length > 32) return 'At most 32 parameters are supported.'
  for (const [name, p] of entries) {
    if (!/^[A-Za-z_][A-Za-z0-9_.-]{0,62}$/.test(name)) return `Invalid parameter name "${name.slice(0, 64)}".`
    if (typeof p !== 'object' || p === null || Array.isArray(p)) return `Parameter "${name}" must be an object.`
    const spec = p as Record<string, unknown>
    const unknown = Object.keys(spec).filter((k) => !['type', 'min', 'max', 'values', 'scale', 'step'].includes(k))
    if (unknown.length > 0) return `Parameter "${name}" has unknown field "${unknown[0]}".`
    if (spec.type === 'float' || spec.type === 'int') {
      const { min, max } = spec
      if (typeof min !== 'number' || typeof max !== 'number' || !Number.isFinite(min) || !Number.isFinite(max)) {
        return `Parameter "${name}" needs numeric min and max.`
      }
      if (min >= max) return `Parameter "${name}": min must be less than max.`
      if (spec.scale !== undefined && spec.scale !== 'linear' && spec.scale !== 'log') return `Parameter "${name}": scale must be "linear" or "log".`
      if (spec.scale === 'log' && min <= 0) return `Parameter "${name}": log scale needs min greater than 0.`
      if (spec.step !== undefined && (typeof spec.step !== 'number' || !Number.isInteger(spec.step) || spec.step < 1)) {
        return `Parameter "${name}": step must be a positive integer.`
      }
    } else if (spec.type === 'choice') {
      const vals = spec.values
      if (!Array.isArray(vals) || vals.length < 1 || vals.length > 64) return `Parameter "${name}" needs 1 to 64 values.`
      if (vals.some((v) => v === null || typeof v === 'object')) return `Parameter "${name}" values must be numbers, strings or booleans.`
    } else {
      return `Parameter "${name}": type must be float, int or choice.`
    }
  }
  return null
}
