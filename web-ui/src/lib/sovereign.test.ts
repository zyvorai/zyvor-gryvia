// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { product, proposalIdIn, stateTone, zyntraDecisionLink, zyntraLink, zyntraSummary, type SovereignStatus } from './sovereign'

describe('stateTone', () => {
  it('maps the gateway states', () => {
    expect(stateTone('healthy')).toBe('ok')
    expect(stateTone('not installed')).toBe('')
    expect(stateTone('unhealthy (503)')).toBe('warn')
    expect(stateTone('unreachable')).toBe('bad')
  })
})

describe('links', () => {
  it('builds Zyntra hash routes and tolerates a trailing slash', () => {
    expect(zyntraLink('https://zyntra.example/', 'approvals')).toBe('https://zyntra.example/#/approvals')
    expect(zyntraDecisionLink('https://zyntra.example', 'prop-ab12')).toBe('https://zyntra.example/#/decision/prop-ab12')
  })
  it('gives no link without a console URL', () => {
    expect(zyntraLink(null, 'gaps')).toBeUndefined()
    expect(zyntraDecisionLink('', 'prop-1')).toBeUndefined()
  })
})

describe('proposalIdIn', () => {
  it('finds the proposal an agent made', () => {
    expect(proposalIdIn('Proposal prop-9f3a2c (Raise priority) is pending.')).toBe('prop-9f3a2c')
    expect(proposalIdIn('I filed prop-00ff for review')).toBe('prop-00ff')
  })
  it('ignores answers without one', () => {
    expect(proposalIdIn('No proposal was made.')).toBeUndefined()
    expect(proposalIdIn('see properties')).toBeUndefined()
  })
})

describe('summaries', () => {
  const s: SovereignStatus = {
    enabled: true,
    products: [
      { name: 'Zyntra', role: '', installed: true, state: 'healthy', version: '0.4.0', executeMode: 'apply', sources: { total: 6, healthy: 5 } },
      { name: 'Netra', role: '', installed: false, state: 'not installed' },
    ],
  }
  it('picks a product by name', () => {
    expect(product(s, 'Netra')?.state).toBe('not installed')
    expect(product(undefined, 'Zyntra')).toBeUndefined()
  })
  it('summarises Zyntra', () => {
    expect(zyntraSummary(product(s, 'Zyntra')!)).toBe('v0.4.0 · applies approved changes · 5/6 sources healthy')
    expect(zyntraSummary({ name: 'Zyntra', role: '', installed: true, state: 'healthy', executeMode: 'dry-run' })).toBe('approved changes: dry-run')
  })
})
