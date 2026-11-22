// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest'
import { NO_FILTERS, actorLabel, auditParams, outcomeTone } from './audit'

describe('audit', () => {
  it('sends only the filters that are set', () => {
    expect(auditParams(NO_FILTERS)).toEqual({ limit: 200 })
    expect(auditParams({ ...NO_FILTERS, outcome: 'denied', actor: ' bob ', since: '2026-01-02' }, 50)).toEqual({
      limit: 50,
      outcome: 'denied',
      actor: 'bob',
      since: '2026-01-02T00:00:00.000Z',
    })
  })
  it('labels actors', () => {
    expect(actorLabel({ auth: 'oidc', role: 'tenant', tenant: 'alpha', subject: 'a@x' })).toBe('a@x (alpha)')
    expect(actorLabel({ auth: 'api_key', role: 'admin', tenant: '', subject: '' })).toBe('admin via api_key')
    expect(actorLabel({ auth: 'none', role: 'none', tenant: '', subject: '' })).toBe('anonymous')
  })
  it('tones outcomes', () => {
    expect(outcomeTone('success')).toBe('ok')
    expect(outcomeTone('denied')).toBe('warn')
    expect(outcomeTone('error')).toBe('bad')
  })
})
