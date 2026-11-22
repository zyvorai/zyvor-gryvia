// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest'
import { applyFilters, applySearch, applySort, filterOptions, paginate, paramsFromQuery, queryFromParams, type FilterDef } from './tableState'

type Row = { name: string; phase: string; gpus?: number }
const rows: Row[] = [
  { name: 'train-a', phase: 'Running', gpus: 8 },
  { name: 'train-b', phase: 'Failed', gpus: 2 },
  { name: 'eval-1', phase: 'Running' },
  { name: 'train-10', phase: 'Pending', gpus: 16 },
]
const statusDef: FilterDef<Row> = { name: 'status', label: 'Status', get: (r) => r.phase }

describe('search and filter', () => {
  it('matches every term, case-insensitively', () => {
    expect(applySearch(rows, 'TRAIN run', (r) => `${r.name} ${r.phase}`).map((r) => r.name)).toEqual(['train-a'])
    expect(applySearch(rows, '  ', (r) => r.name)).toBe(rows)
  })

  it('filters by exact value and builds options from the data', () => {
    expect(applyFilters(rows, { status: 'running' }, [statusDef]).map((r) => r.name)).toEqual(['train-a', 'eval-1'])
    expect(filterOptions(rows, statusDef)).toEqual(['Failed', 'Pending', 'Running'])
  })
})

describe('sort', () => {
  const acc = { name: (r: Row) => r.name, gpus: (r: Row) => r.gpus }

  it('sorts numerically and naturally, missing values last in both directions', () => {
    expect(applySort(rows, 'gpus', 'asc', acc).map((r) => r.name)).toEqual(['train-b', 'train-a', 'train-10', 'eval-1'])
    expect(applySort(rows, 'gpus', 'desc', acc).map((r) => r.name)).toEqual(['train-10', 'train-a', 'train-b', 'eval-1'])
    expect(applySort(rows, 'name', 'asc', acc).map((r) => r.name)).toEqual(['eval-1', 'train-10', 'train-a', 'train-b'])
  })

  it('leaves order alone for an unknown key', () => {
    expect(applySort(rows, 'nope', 'asc', acc)).toBe(rows)
  })
})

describe('paginate', () => {
  it('clamps the page and reports the visible range', () => {
    expect(paginate(rows, 1, 3)).toMatchObject({ page: 1, pageCount: 2, from: 1, to: 3 })
    expect(paginate(rows, 9, 3)).toMatchObject({ page: 2, from: 4, to: 4 })
    expect(paginate([], 1, 3)).toMatchObject({ page: 1, pageCount: 1, from: 0, to: 0 })
  })
})

describe('URL round trip', () => {
  const defs = [{ name: 'status' }]
  const defaults = { sortKey: 'name', sortDir: 'asc' as const, pageSize: 25 }

  it('reads and writes only non-default state', () => {
    const q = queryFromParams(new URLSearchParams('q=train&f_status=Running&sort=gpus&dir=desc&page=2'), defs, defaults)
    expect(q).toMatchObject({ search: 'train', filters: { status: 'Running' }, sortKey: 'gpus', sortDir: 'desc', page: 2 })
    expect(paramsFromQuery(q, new URLSearchParams(), defs, defaults).toString()).toBe('q=train&f_status=Running&sort=gpus&dir=desc&page=2')
    const d = queryFromParams(new URLSearchParams(), defs, defaults)
    expect(paramsFromQuery(d, new URLSearchParams('other=1'), defs, defaults).toString()).toBe('other=1')
  })

  it('ignores garbage', () => {
    expect(queryFromParams(new URLSearchParams('page=-4&dir=sideways'), defs, defaults)).toMatchObject({ page: 1, sortDir: 'asc' })
  })
})
