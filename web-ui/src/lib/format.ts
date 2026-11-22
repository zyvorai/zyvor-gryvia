// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Number, date and size formatting shared by every page.

const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

/** "3m ago" style relative time; falls back to a dash for missing/invalid input. */
export function formatRelative(iso?: string | number | Date | null, now: number = Date.now()): string {
  if (iso === undefined || iso === null || iso === '') return '—'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return '—'
  const diff = now - t
  if (diff < 0) return 'just now'
  if (diff < MINUTE) return 'just now'
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)}m ago`
  if (diff < DAY) return `${Math.floor(diff / HOUR)}h ago`
  if (diff < 30 * DAY) return `${Math.floor(diff / DAY)}d ago`
  return formatDate(iso)
}

/** Absolute date-time, including the year. */
export function formatDate(iso?: string | number | Date | null): string {
  if (iso === undefined || iso === null || iso === '') return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

/** "2d 3h", "5h 12m", "42s"; safe for missing, invalid or negative input. */
export function formatDuration(ms?: number | null): string {
  if (ms === undefined || ms === null || !Number.isFinite(ms) || ms < 0) return '—'
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`.replace(/ 0s$/, '')
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ${m % 60}m`.replace(/ 0m$/, '')
  const d = Math.floor(h / 24)
  return `${d}d ${h % 24}h`.replace(/ 0h$/, '')
}

/** Duration between two timestamps; open-ended (still running) uses `now`. */
export function durationBetween(start?: string, end?: string, now: number = Date.now()): string {
  if (!start) return '—'
  const s = new Date(start).getTime()
  const e = end ? new Date(end).getTime() : now
  if (Number.isNaN(s) || Number.isNaN(e)) return '—'
  return formatDuration(e - s)
}

/** Binary bytes: 1.5 GiB style, labelled KB/MB/GB for familiarity. */
export function formatBytes(n?: number | null): string {
  if (n === undefined || n === null || !Number.isFinite(n)) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let v = Math.abs(n)
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i += 1
  }
  const digits = i === 0 || v >= 100 ? 0 : v >= 10 ? 1 : 2
  return `${n < 0 ? '-' : ''}${v.toFixed(digits)} ${units[i]}`
}

const money = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2 })
const moneyPrecise = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 4 })

/** USD with cents; values under a cent keep extra digits so small costs don't read as $0.00. */
export function formatMoney(n?: number | null): string {
  if (n === undefined || n === null || !Number.isFinite(n)) return '—'
  if (n !== 0 && Math.abs(n) < 0.01) return moneyPrecise.format(n)
  return money.format(n)
}

/** Percent with at most one decimal; input is already 0–100. */
export function formatPercent(n?: number | null): string {
  if (n === undefined || n === null || !Number.isFinite(n)) return '—'
  return `${Number.isInteger(n) ? n : n.toFixed(1)}%`
}

/** Integer or one-decimal number with thousands grouping. */
export function formatNumber(n?: number | null): string {
  if (n === undefined || n === null || !Number.isFinite(n)) return '—'
  return new Intl.NumberFormat('en-US', { maximumFractionDigits: Number.isInteger(n) ? 0 : 1 }).format(n)
}
