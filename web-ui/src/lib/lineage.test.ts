// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { columns, neighbours, reach, type LineageGraph } from './lineage'

const n = (kind: 'dataset' | 'job' | 'model' | 'service', name: string) => ({ id: `${kind}/ns/${name}`, kind, name, namespace: 'ns' })
const g: LineageGraph = {
  nodes: [n('service', 's'), n('model', 'm'), n('job', 'j'), n('dataset', 'd')],
  edges: [
    { from: 'dataset/ns/d', to: 'job/ns/j' },
    { from: 'job/ns/j', to: 'model/ns/m' },
    { from: 'model/ns/m', to: 'service/ns/s' },
  ],
}

describe('lineage', () => {
  it('groups nodes in pipeline order', () => {
    expect(columns(g).map((c) => c.kind)).toEqual(['dataset', 'job', 'model', 'service'])
  })
  it('finds direct neighbours', () => {
    const r = neighbours(g, 'job/ns/j')
    expect(r.upstream.map((x) => x.name)).toEqual(['d'])
    expect(r.downstream.map((x) => x.name)).toEqual(['m'])
  })
  it('walks transitively and terminates on cycles', () => {
    expect(reach(g, 'dataset/ns/d', 'down').map((x) => x.name).sort()).toEqual(['j', 'm', 's'])
    expect(reach(g, 'service/ns/s', 'up').map((x) => x.name).sort()).toEqual(['d', 'j', 'm'])
    const cyc = { ...g, edges: [...g.edges, { from: 'service/ns/s', to: 'dataset/ns/d' }] }
    expect(reach(cyc, 'job/ns/j', 'down').map((x) => x.name).sort()).toEqual(['d', 'm', 's'])
  })
})
