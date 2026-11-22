// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest'
import { formatColumns, parseColumns, parseTags, usedByLabel } from './dataCatalog'

describe('dataCatalog', () => {
  it('parses and formats columns', () => {
    const cols = parseColumns('id: int\n\n  email: string - contact address \nnotes')
    expect(cols).toEqual([{ name: 'id', type: 'int' }, { name: 'email', type: 'string', description: 'contact address' }, { name: 'notes' }])
    expect(parseColumns(formatColumns(cols))).toEqual(cols)
    expect(parseColumns('ts: map<string, int>')[0].type).toBe('map<string, int>')
  })
  it('normalises tags', () => {
    expect(parseTags(' PII, sales ,pii,, ')).toEqual(['pii', 'sales'])
    expect(parseTags('')).toEqual([])
  })
  it('summarises what uses a dataset', () => {
    expect(usedByLabel([])).toBe('Not used yet')
    expect(usedByLabel(['job/a', 'job/b', 'model/m', 'service/s'])).toBe('2 jobs, 1 model, 1 service')
  })
})
