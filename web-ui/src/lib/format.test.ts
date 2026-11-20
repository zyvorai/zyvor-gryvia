// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { durationBetween, formatBytes, formatDuration, formatMoney, formatPercent, formatRelative } from './format'

const NOW = new Date('2026-09-29T12:00:00Z').getTime()

describe('formatRelative', () => {
  it('handles recent, hours, days and bad input', () => {
    expect(formatRelative('2026-09-29T11:59:30Z', NOW)).toBe('just now')
    expect(formatRelative('2026-09-29T11:15:00Z', NOW)).toBe('45m ago')
    expect(formatRelative('2026-09-29T07:00:00Z', NOW)).toBe('5h ago')
    expect(formatRelative('2026-09-26T12:00:00Z', NOW)).toBe('3d ago')
    expect(formatRelative('not-a-date', NOW)).toBe('—')
    expect(formatRelative(undefined, NOW)).toBe('—')
  })
})

describe('formatDuration', () => {
  it('covers seconds through days and invalid values', () => {
    expect(formatDuration(42_000)).toBe('42s')
    expect(formatDuration(5 * 60_000)).toBe('5m')
    expect(formatDuration(5 * 3_600_000 + 12 * 60_000)).toBe('5h 12m')
    expect(formatDuration(52 * 3_600_000 + 3 * 60_000)).toBe('2d 4h')
    expect(formatDuration(-1)).toBe('—')
    expect(formatDuration(NaN)).toBe('—')
  })

  it('computes between timestamps and copes with invalid ones', () => {
    expect(durationBetween('2026-09-29T10:00:00Z', '2026-09-29T11:30:00Z', NOW)).toBe('1h 30m')
    expect(durationBetween('2026-09-29T10:00:00Z', undefined, NOW)).toBe('2h')
    expect(durationBetween(undefined, undefined, NOW)).toBe('—')
    expect(durationBetween('nope', undefined, NOW)).toBe('—')
  })
})

describe('formatBytes / formatMoney / formatPercent', () => {
  it('formats sizes', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1536)).toBe('1.50 KB')
    expect(formatBytes(5 * 1024 ** 3)).toBe('5.00 GB')
    expect(formatBytes(undefined)).toBe('—')
  })

  it('formats money without losing sub-cent amounts', () => {
    expect(formatMoney(1234.5)).toBe('$1,234.50')
    expect(formatMoney(0)).toBe('$0.00')
    expect(formatMoney(0.0042)).toBe('$0.0042')
    expect(formatMoney(NaN)).toBe('—')
  })

  it('formats percents', () => {
    expect(formatPercent(53)).toBe('53%')
    expect(formatPercent(52.5)).toBe('52.5%')
    expect(formatPercent(undefined)).toBe('—')
  })
})

describe('formatNumber', () => {
  it('groups thousands and handles missing values', async () => {
    const { formatNumber } = await import('./format')
    expect(formatNumber(1234567)).toBe('1,234,567')
    expect(formatNumber(12.34)).toBe('12.3')
    expect(formatNumber(undefined)).toBe('—')
  })
})
