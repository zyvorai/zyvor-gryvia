// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { emptyRow, initialSpaceState, jsonToRows, parseChipValue, rowsToSpace, spaceError, spaceJson, validateRows, type ParamRow, withJson, withRows } from './paramSpace'
import { validateParameterSpace } from './tuner'

const noId = (r: ParamRow) => ({ ...r, id: '' })

describe('rowsToSpace', () => {
  it('produces the gateway shape', () => {
    const space = rowsToSpace([
      emptyRow({ name: 'lr', type: 'float', min: '1e-5', max: '0.01', scale: 'log' }),
      emptyRow({ name: 'layers', type: 'int', min: '1', max: '8', step: '1' }),
      emptyRow({ name: 'bs', type: 'choice', values: ['16', '32', 'adam', 'true'] }),
    ])
    expect(space).toEqual({
      lr: { type: 'float', min: 1e-5, max: 0.01, scale: 'log' },
      layers: { type: 'int', min: 1, max: 8, step: 1 },
      bs: { type: 'choice', values: [16, 32, 'adam', true] },
    })
    expect(validateParameterSpace(JSON.stringify(space))).toBeNull()
  })
  it('leaves out unfinished numbers and default scale', () => {
    expect(rowsToSpace([emptyRow({ name: 'a', min: '', max: '2' })])).toEqual({ a: { type: 'float', max: 2 } })
  })
})

describe('jsonToRows', () => {
  it('round-trips rows through JSON', () => {
    const state = initialSpaceState()
    const parsed = jsonToRows(state.json)
    expect('rows' in parsed).toBe(true)
    if ('rows' in parsed) {
      expect(parsed.rows.map(noId)).toEqual(state.rows.map(noId))
      expect(JSON.parse(spaceJson(withRows(state, parsed.rows)))).toEqual(JSON.parse(state.json))
    }
  })
  it('reports why it cannot', () => {
    expect(jsonToRows('{')).toHaveProperty('error')
    expect(jsonToRows('{"a":{"type":"float","min":2,"max":1}}')).toHaveProperty('error')
  })
})

describe('validateRows', () => {
  it('checks name, uniqueness, range, log scale and step', () => {
    const errs = validateRows([
      emptyRow({ name: '1bad', min: '0', max: '1' }),
      emptyRow({ name: 'dup', min: '0', max: '1' }),
      emptyRow({ name: 'dup', min: '2', max: '1' }),
      emptyRow({ name: 'lg', min: '0', max: '1', scale: 'log' }),
      emptyRow({ name: 'st', min: '0', max: '9', step: '1.5' }),
      emptyRow({ name: 'ch', type: 'choice' }),
    ])
    expect(errs[0].name).toBeDefined()
    expect(errs[1].name).toMatch(/unique/)
    expect(errs[2].max).toMatch(/greater/)
    expect(errs[3].min).toMatch(/Log/)
    expect(errs[4].step).toBeDefined()
    expect(errs[5].values).toBeDefined()
  })
  it('accepts a good row', () => {
    expect(validateRows([emptyRow({ name: 'lr', min: '0.001', max: '0.1', scale: 'log' })])).toEqual([{}])
  })
})

describe('state sync', () => {
  it('keeps json in step with rows and rows in step with valid json', () => {
    const s = initialSpaceState([emptyRow({ name: 'a', min: '0', max: '1' })])
    const edited = withRows(s, [emptyRow({ name: 'b', min: '0', max: '5' })])
    expect(JSON.parse(edited.json)).toEqual({ b: { type: 'float', min: 0, max: 5 } })
    const back = withJson(edited, '{"c":{"type":"choice","values":["x","y"]}}')
    expect(back.rows[0]).toMatchObject({ name: 'c', type: 'choice', values: ['x', 'y'] })
    const broken = withJson(back, '{"c":')
    expect(broken.rows).toBe(back.rows)
    expect(broken.json).toBe('{"c":')
  })
  it('spaceError follows rows or json depending on the mode', () => {
    const ok = initialSpaceState()
    expect(spaceError(ok)).toBeNull()
    expect(spaceError({ ...ok, rows: [] })).toMatch(/at least one/)
    expect(spaceError({ ...ok, advanced: true, json: '[]' })).toMatch(/object/)
    expect(spaceError(withRows(ok, [emptyRow({ name: '' })]))).toMatch(/Fix parameter 1/)
  })
  it('parses chip values', () => {
    expect(parseChipValue('0.5')).toBe(0.5)
    expect(parseChipValue('1e-3')).toBe(0.001)
    expect(parseChipValue('adam')).toBe('adam')
    expect(parseChipValue('false')).toBe(false)
  })
})
