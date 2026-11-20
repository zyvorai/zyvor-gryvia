// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { MAX_HISTORY, historyFor, toolsLabel } from './copilot'

describe('copilot', () => {
  it('sends only answered turns, newest MAX_HISTORY', () => {
    const turns = [
      { role: 'user' as const, content: 'a' },
      { role: 'assistant' as const, content: '', error: 'boom' },
      { role: 'assistant' as const, content: 'b', tools: ['list_jobs'] },
    ]
    expect(historyFor(turns)).toEqual([
      { role: 'user', content: 'a' },
      { role: 'assistant', content: 'b' },
    ])
    const many = Array.from({ length: 30 }, (_, i) => ({ role: 'user' as const, content: String(i) }))
    expect(historyFor(many)).toHaveLength(MAX_HISTORY)
    expect(historyFor(many)[0].content).toBe('10')
  })
  it('labels the tools used once each', () => {
    expect(toolsLabel(['list_jobs', 'get_lineage', 'list_jobs'])).toBe('Looked at jobs, lineage')
    expect(toolsLabel([])).toBe('')
    expect(toolsLabel(['x'])).toBe('Looked at x')
  })
})
