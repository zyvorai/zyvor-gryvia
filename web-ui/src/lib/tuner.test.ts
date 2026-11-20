// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { bestAcross, formatMetric, formatNumber, validateParameterSpace } from './tuner'

describe('formatMetric', () => {
  it('adds percent only for ratio-like metrics within 0..1', () => {
    expect(formatMetric(0.9234, 'accuracy')).toBe('92.34%')
    expect(formatMetric(0.5, 'val_f1')).toBe('50%')
    expect(formatMetric(1.7, 'accuracy')).toBe('1.7')
    expect(formatMetric(0.0312, 'loss')).toBe('0.0312')
  })
  it('keeps up to 4 significant digits and handles negatives and missing', () => {
    expect(formatMetric(-1.23456, 'reward')).toBe('-1.235')
    expect(formatNumber(123456)).toBe('123,456')
    expect(formatMetric(undefined, 'loss')).toBe('—')
    expect(formatMetric(Number.NaN)).toBe('—')
  })
})

describe('bestAcross', () => {
  const t = (v: number | undefined, direction?: string, metricName = 'loss') => ({ spec: { direction, metricName }, status: { bestMetricValue: v } })
  it('respects minimize and negative values', () => {
    expect(bestAcross([t(0.5, 'minimize'), t(0.2, 'minimize')]).value).toBe(0.2)
    expect(bestAcross([t(-3, 'maximize', 'reward'), t(-1, 'maximize', 'reward')]).value).toBe(-1)
  })
  it('reports mixed objectives and empty input', () => {
    expect(bestAcross([t(1, 'minimize'), t(2, 'maximize')]).mixed).toBe(true)
    expect(bestAcross([t(undefined)]).value).toBeUndefined()
    expect(bestAcross(undefined).mixed).toBe(false)
  })
})

describe('validateParameterSpace', () => {
  it('accepts a valid space', () => {
    expect(validateParameterSpace('{"lr":{"type":"float","min":0.001,"max":0.1},"bs":{"type":"choice","values":[16,32]}}')).toBeNull()
  })
  it('flags problems', () => {
    expect(validateParameterSpace('{')).toMatch(/JSON/)
    expect(validateParameterSpace('[]')).toMatch(/object/)
    expect(validateParameterSpace('{"a":{"type":"float","min":1,"max":1}}')).toMatch(/min/)
    expect(validateParameterSpace('{"a":{"type":"choice","values":[]}}')).toMatch(/values/)
    expect(validateParameterSpace('{"a":{"type":"str"}}')).toMatch(/type/)
  })
})
