// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import type { SecurityPolicy } from './api'
import { activeRuleTypes, countActivePolicies, policyCounters, ruleName } from './security'

const pol = (phase: string | undefined, rules: Array<[string, boolean]>, status: Partial<NonNullable<SecurityPolicy['status']>> = {}): SecurityPolicy => ({
  metadata: { name: 'p' },
  spec: { targetNamespaces: [], autoBlock: false, detectionRules: rules.map(([type, enabled]) => ({ type, enabled, sensitivity: 'high' })) },
  status: phase ? { phase, activeDetections: 0, alertsTriggered: 0, ...status } : undefined,
})

describe('security helpers', () => {
  it('names rules', () => {
    expect(ruleName('driver_fim')).toBe('Driver file integrity')
    expect(ruleName('some_new_rule')).toBe('Some new rule')
  })
  it('counts coverage only from Active policies', () => {
    const ps = [pol('Active', [['escape', true], ['mining', false]]), pol('Pending', [['privesc', true]]), pol(undefined, [['mining', true]])]
    expect([...activeRuleTypes(ps)]).toEqual(['escape'])
    expect(countActivePolicies(ps)).toBe(1)
  })
  it('aggregates counters', () => {
    const c = policyCounters([pol('Active', [], { alertsTriggered: 2, lastAlert: '2026-01-01T00:00:00Z', detectionCounts: { escape: 2 } }), pol('Active', [], { alertsTriggered: 1, lastAlert: '2026-02-01T00:00:00Z', detectionCounts: { escape: 1, mining: 5 } })])
    expect(c.alerts).toBe(3)
    expect(c.lastAlert).toBe('2026-02-01T00:00:00Z')
    expect(c.byType[0]).toEqual(['mining', 5])
  })
})

import { buildSecurityRequest, defaultRuleDrafts, validateNamespaceChip, validateSecurityForm, validateWebhook } from './security'

describe('security policy form', () => {
  const rules = defaultRuleDrafts().map((r, i) => ({ ...r, enabled: i === 0 }))
  const ok = { name: 'gpu-guard', namespaces: ['ml'], rules, webhook: '', autoBlock: false }
  it('accepts a valid form and builds the gateway request', () => {
    expect(validateSecurityForm(ok)).toEqual({})
    const req = buildSecurityRequest({ ...ok, webhook: ' https://hooks.example.com/a ', autoBlock: true })
    expect(req.alertWebhook).toBe('https://hooks.example.com/a')
    expect(req.autoBlock).toBe(true)
    expect(req.detectionRules).toHaveLength(5)
    expect(buildSecurityRequest(ok)).not.toHaveProperty('alertWebhook')
  })
  it('requires name, namespaces and one enabled rule', () => {
    const e = validateSecurityForm({ name: 'Bad Name', namespaces: [], rules: defaultRuleDrafts(), webhook: '', autoBlock: false })
    expect(e.name).toBeTruthy()
    expect(e.namespaces).toBeTruthy()
    expect(e.rules).toBeTruthy()
  })
  it('validates namespace chips', () => {
    expect(validateNamespaceChip('ml', [])).toBeUndefined()
    expect(validateNamespaceChip('ML', [])).toBeTruthy()
    expect(validateNamespaceChip('', [])).toBeTruthy()
    expect(validateNamespaceChip('ml', ['ml'])).toMatch(/already/)
  })
  it('validates the webhook like the gateway', () => {
    expect(validateWebhook('')).toBeUndefined()
    expect(validateWebhook('http://x.io/h')).toBeUndefined()
    expect(validateWebhook('ftp://x.io')).toBeTruthy()
    expect(validateWebhook('not a url')).toBeTruthy()
  })
})
