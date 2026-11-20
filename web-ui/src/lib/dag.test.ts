// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { dagLayers, describeSteps, validateDag } from './dag'

const names = (layers: { name: string }[][]) => layers.map((l) => l.map((s) => s.name))

describe('dagLayers', () => {
  it('stacks independent steps in one level and chains dependents', () => {
    const layers = dagLayers([{ name: 'a' }, { name: 'b' }, { name: 'c', dependsOn: ['a', 'b'] }, { name: 'd', dependsOn: ['c'] }])
    expect(names(layers)).toEqual([['a', 'b'], ['c'], ['d']])
  })
  it('uses the deepest dependency', () => {
    const layers = dagLayers([{ name: 'a' }, { name: 'b', dependsOn: ['a'] }, { name: 'c', dependsOn: ['a', 'b'] }])
    expect(names(layers)).toEqual([['a'], ['b'], ['c']])
  })
  it('ignores unknown deps and survives cycles', () => {
    expect(names(dagLayers([{ name: 'a', dependsOn: ['ghost'] }]))).toEqual([['a']])
    const cyc = dagLayers([{ name: 'a', dependsOn: ['b'] }, { name: 'b', dependsOn: ['a'] }])
    expect(cyc.flat()).toHaveLength(2)
  })
  it('handles empty input', () => {
    expect(dagLayers([])).toEqual([])
  })
})


describe('validateDag', () => {
  it('accepts a valid graph', () => {
    expect(validateDag([{ name: 'a' }, { name: 'b', dependsOn: ['a'] }])).toEqual([])
  })
  it('reports duplicates, unknown and self dependencies', () => {
    const issues = validateDag([{ name: 'a' }, { name: 'a' }, { name: 'b', dependsOn: ['ghost', 'b'] }])
    expect(issues.map((i) => i.kind).sort()).toEqual(['duplicate', 'self', 'unknown'])
    expect(issues.find((i) => i.kind === 'unknown')?.steps).toEqual(['b'])
  })
  it('reports cycles with their members and not steps that merely feed them', () => {
    const issues = validateDag([{ name: 'root' }, { name: 'a', dependsOn: ['root', 'b'] }, { name: 'b', dependsOn: ['a'] }])
    expect(issues).toHaveLength(1)
    expect(issues[0].kind).toBe('cycle')
    expect(issues[0].steps.sort()).toEqual(['a', 'b'])
  })
  it('ignores blank names', () => {
    expect(validateDag([{ name: '' }, { name: '' }])).toEqual([])
  })
})

describe('describeSteps', () => {
  it('lists steps in execution order with stage, dependencies and extra detail', () => {
    const out = describeSteps([{ name: 'deploy', dependsOn: ['train', 'ghost'] }, { name: 'train', dependsOn: ['prep'] }, { name: 'prep' }], (s) => (s.name === 'prep' ? 'Succeeded' : undefined))
    expect(out).toEqual(['prep: stage 1, no dependencies, Succeeded', 'train: stage 2, runs after prep', 'deploy: stage 3, runs after train'])
  })
})
