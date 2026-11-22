// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest'
import { engineLabel, formatCount, formatMs, formatRatio, inferenceRows, NOT_MEASURED } from './inference'

describe('formatMs', () => {
  it('sizes the unit', () => {
    expect(formatMs(0.86)).toBe('0.86 ms')
    expect(formatMs(72.222)).toBe('72.2 ms')
    expect(formatMs(475)).toBe('475 ms')
    expect(formatMs(2250)).toBe('2.25 s')
  })
  it('never turns a missing or invalid figure into a number', () => {
    for (const v of [undefined, NaN, Infinity, -1]) expect(formatMs(v)).toBe(NOT_MEASURED)
  })
})

describe('inferenceRows', () => {
  const base = { namespace: 'ml', job: 'serve', available: true }
  it('always lists TTFT, ITL, queue and e2e, marking unexported ones', () => {
    const rows = inferenceRows({ ...base, engine: 'tgi', queueTimeP99ms: 35, itlP99ms: 125, e2eP99ms: 15000 })
    expect(rows.map((r) => [r.key, r.value, r.measured])).toEqual([
      ['ttft', NOT_MEASURED, false],
      ['itl', '125 ms', true],
      ['queue', '35 ms', true],
      ['e2e', '15 s', true],
    ])
    expect(rows[1].hint).toMatch(/per-request mean/)
  })
  it('shows means for counter-only engines, labelled as means', () => {
    const rows = inferenceRows({ ...base, engine: 'triton', queueTimeMeanMs: 0.86, e2eMeanMs: 7.4 })
    expect(rows.find((r) => r.key === 'queue')?.measured).toBe(false)
    const mean = rows.find((r) => r.key === 'queueMean')
    expect(mean?.label).toMatch(/mean/)
    expect(mean?.value).toBe('0.86 ms')
    expect(rows.find((r) => r.key === 'e2eMean')?.value).toBe('7.4 ms')
  })
  it('does not add a mean row next to a real p99', () => {
    const rows = inferenceRows({ ...base, engine: 'triton', queueTimeP99ms: 0.9, queueTimeMeanMs: 0.86 })
    expect(rows.some((r) => r.key === 'queueMean')).toBe(false)
  })
})

describe('other formatters', () => {
  it('formats counts and ratios', () => {
    expect(formatCount(2.6)).toBe('3')
    expect(formatCount(undefined)).toBe(NOT_MEASURED)
    expect(formatRatio(0.42)).toBe('42%')
    expect(formatRatio(1.7)).toBe('100%')
    expect(formatRatio(undefined)).toBe(NOT_MEASURED)
  })
  it('only names known engines', () => {
    expect(engineLabel('vllm')).toBe('vLLM')
    expect(engineLabel('<img src=x>')).toBe('Unknown engine')
  })
})
