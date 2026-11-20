// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { collectorState, formatNs, humanize, normalizeBottleneck, ratioPercent, slowdownLabel } from './gpuComm'

describe('gpuComm helpers', () => {
  it('classifies collectors', () => {
    expect(collectorState(undefined)).toBe('unknown')
    expect(collectorState({ reachable: 0, total: 0 })).toBe('none-deployed')
    expect(collectorState({ reachable: 0, total: 3 })).toBe('unreachable')
    expect(collectorState({ reachable: 2, total: 3 })).toBe('partial')
    expect(collectorState({ reachable: 3, total: 3 })).toBe('ok')
  })
  it('normalises bottleneck', () => {
    expect(normalizeBottleneck(' Data_Loading ')).toBe('data_loading')
    expect(humanize('data_loading_wait')).toBe('data loading wait')
  })
  it('treats ratio > 1 as percent', () => {
    expect(ratioPercent(0.42)).toBeCloseTo(42)
    expect(ratioPercent(42)).toBe(42)
    expect(ratioPercent(NaN)).toBeUndefined()
  })
  it('guards slowdown', () => {
    expect(slowdownLabel(2.44)).toBe('2.4x slower')
    expect(slowdownLabel(NaN)).toBeUndefined()
    expect(slowdownLabel(0)).toBeUndefined()
  })
  it('formats ns', () => {
    expect(formatNs(1500)).toBe('1.5us')
    expect(formatNs(undefined)).toBe('—')
  })
})
