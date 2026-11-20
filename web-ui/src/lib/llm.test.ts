// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { curlExample, keyNameError, priceLabel, quotaPercent, quotaTone } from './llm'

describe('llm helpers', () => {
  it('validates key names', () => {
    expect(keyNameError('ci-runner')).toBeUndefined()
    expect(keyNameError('')).toBe('Name is required')
    expect(keyNameError('CI')).toMatch(/Lowercase/)
    expect(keyNameError('-x')).toMatch(/Lowercase/)
    expect(keyNameError('a'.repeat(64))).toMatch(/Lowercase/)
  })

  it('labels prices', () => {
    expect(priceLabel({ priceInputPer1M: 0.5, priceOutputPer1M: null })).toBe('0.50 / —')
  })

  it('computes quota use', () => {
    const q = { name: 'a', namespaces: ['a'], tokensPerDay: 1000, usedToday: 805 }
    expect(quotaPercent(q)).toBe(80.5)
    expect(quotaTone(q)).toBe('warn')
    expect(quotaTone({ ...q, usedToday: 2000 })).toBe('bad')
    expect(quotaPercent({ ...q, usedToday: 2000 })).toBe(100)
    expect(quotaTone({ ...q, usedToday: 10 })).toBe('ok')
    expect(quotaPercent({ ...q, tokensPerDay: 0 })).toBe(0)
  })

  it('builds a curl example', () => {
    expect(curlExample('http://gw:8080', 'chat')).toContain('http://gw:8080/v1/chat/completions')
    expect(curlExample('', 'chat', 'gk-1')).toContain('Bearer gk-1')
    expect(curlExample('', 'chat')).toContain('"model":"chat"')
  })
})
