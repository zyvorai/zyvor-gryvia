// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { bucketPolicies, edgeGeometry, filterFlows, flowFilterOptions, formatCaptureLevel, pageWindow, policyBucket, twoWayEdges, validatePolicyForm, verdictKind, verdictTone, EMPTY_FLOW_FILTERS } from './network'

describe('edge geometry', () => {
  it('keeps A->B and B->A apart', () => {
    const a = { x: 0, y: 0 }
    const b = { x: 200, y: 0 }
    const ab = edgeGeometry(a, b, 100, 50, 8)
    const ba = edgeGeometry(b, a, 100, 50, 8)
    expect(Math.abs(ab.mid.y - ba.mid.y)).toBeCloseTo(16)
    expect(ab.start.x).toBeCloseTo(50)
  })
  it('does not offset without a reverse edge', () => {
    expect(edgeGeometry({ x: 0, y: 0 }, { x: 200, y: 0 }, 100, 50, 0).mid.y).toBe(0)
  })
  it('detects two-way pairs', () => {
    expect(twoWayEdges([{ source: 'a', target: 'b' }, { source: 'b', target: 'a' }, { source: 'a', target: 'c' }])).toEqual([true, true, false])
  })
})

describe('verdicts', () => {
  it('classifies', () => {
    expect(verdictKind('allow')).toBe('good')
    expect(verdictTone('DROP')).toBe('bad')
    expect(verdictTone('ERROR')).toBe('warn')
    expect(verdictTone('FORWARDED')).toBe('')
    expect(verdictKind('')).toBe('unknown')
  })
  it('formats capture level', () => {
    expect(formatCaptureLevel('l7')).toBe('L7')
    expect(formatCaptureLevel('l3_l4')).toBe('L3/L4')
  })
})

const flow = (source: string, destination: string, protocol: string, verdict: string, namespace?: string) => ({ metadata: { namespace }, spec: { source, destination, protocol, verdict } })

describe('flow filters', () => {
  const flows = [flow('Web', 'db', 'TCP', 'FORWARDED', 'prod'), flow('web', 'api', 'tcp', 'ERROR', 'Prod'), flow('x', 'y', 'UDP', 'DROP', 'dev')]
  it('builds options from data, case-insensitively de-duplicated', () => {
    const o = flowFilterOptions(flows)
    expect(o.protocols).toEqual(['TCP', 'UDP'])
    expect(o.verdicts).toEqual(['DROP', 'ERROR', 'FORWARDED'])
    expect(o.namespaces).toEqual(['dev', 'prod'])
  })
  it('filters case-insensitively', () => {
    expect(filterFlows(flows, { ...EMPTY_FLOW_FILTERS, service: 'WEB' })).toHaveLength(2)
    expect(filterFlows(flows, { ...EMPTY_FLOW_FILTERS, namespace: 'PROD', protocol: 'tcp' })).toHaveLength(2)
    expect(filterFlows(flows, { ...EMPTY_FLOW_FILTERS, verdict: 'error' })).toHaveLength(1)
  })
})

describe('pageWindow', () => {
  it('stays inside bounds', () => {
    expect(pageWindow(1, 10)).toEqual([1, 2, 3, 4, 5])
    expect(pageWindow(5, 10)).toEqual([3, 4, 5, 6, 7])
    expect(pageWindow(10, 10)).toEqual([6, 7, 8, 9, 10])
    expect(pageWindow(2, 3)).toEqual([1, 2, 3])
  })
})

describe('policy buckets', () => {
  it('puts missing status in pending and unknown in other', () => {
    expect(policyBucket(undefined)).toBe('pending')
    expect(policyBucket('Active')).toBe('enforced')
    expect(policyBucket('Weird')).toBe('other')
    const b = bucketPolicies(['Enforced', undefined, 'Suggested', 'Failed'], (x) => x)
    expect(Object.values(b).flat()).toHaveLength(4)
  })
})

describe('policy form validation', () => {
  const ok = { name: '', sourceService: 'web', destinationService: 'db', port: '5432', intent: 'allow db' }
  it('accepts valid', () => expect(validatePolicyForm(ok)).toEqual({}))
  it('rejects bad values', () => {
    const e = validatePolicyForm({ name: 'Bad_Name', sourceService: 'Web', destinationService: 'a'.repeat(64), port: '70000', intent: '' })
    expect(Object.keys(e).sort()).toEqual(['destinationService', 'intent', 'name', 'port', 'sourceService'])
  })
})

import { buildTraceDuration, filterFlows as ff, flowsUrlForEdge, flowsUrlForService, graphFocusModel, parseByteQuantity, parseLatencyMs, parsePolicyPrefill, parseTraceTarget, policyPrefillUrl, traceUrl, validateTraceForm } from './network'

describe('trace form', () => {
  const ok = { targetService: 'payment-api', namespace: 'default', captureLevel: 'l4', durationAmount: '5', durationUnit: 'm' }
  it('accepts a valid form', () => {
    expect(validateTraceForm(ok)).toEqual({})
    expect(buildTraceDuration('5', 'm')).toBe('5m')
  })
  it('mirrors the gateway duration rule', () => {
    expect(buildTraceDuration('0', 's')).toBeNull()
    expect(buildTraceDuration('10000', 's')).toBeNull()
    expect(buildTraceDuration('9999', 'h')).toBe('9999h')
    expect(buildTraceDuration('1.5', 'm')).toBeNull()
    expect(buildTraceDuration('5', 'd')).toBeNull()
  })
  it('flags bad names, level and duration', () => {
    const e = validateTraceForm({ targetService: 'Bad_Name', namespace: '', captureLevel: 'l5', durationAmount: '', durationUnit: 'm' })
    expect(e.targetService).toBeTruthy()
    expect(e.namespace).toBeTruthy()
    expect(e.captureLevel).toBeTruthy()
    expect(e.durationAmount).toBeTruthy()
  })
})

describe('url contracts', () => {
  it('builds and parses the policy prefill', () => {
    const url = policyPrefillUrl({ source: 'web', destination: 'db', port: 5432, protocol: 'TCP' })
    expect(url).toBe('/network/policies?new=1&src=web&dst=db&port=5432&protocol=TCP')
    const p = parsePolicyPrefill(new URLSearchParams(url.split('?')[1]))
    expect(p).toEqual({ open: true, sourceService: 'web', destinationService: 'db', port: '5432', protocol: 'TCP' })
  })
  it('falls back safely on junk', () => {
    const p = parsePolicyPrefill(new URLSearchParams('new=1&port=abc&protocol=grpc'))
    expect(p.port).toBe('80')
    expect(p.protocol).toBe('gRPC')
    expect(parsePolicyPrefill(new URLSearchParams('')).open).toBe(false)
  })
  it('links flows and traces', () => {
    expect(flowsUrlForService('a b')).toBe('/network/flows?service=a+b')
    expect(flowsUrlForEdge('a', 'b')).toBe('/network/flows?source=a&destination=b')
    expect(traceUrl('db')).toBe('/network?trace=db')
    expect(parseTraceTarget(new URLSearchParams('trace=db'))).toBe('db')
    expect(parseTraceTarget(new URLSearchParams(''))).toBeNull()
  })
  it('filters by exact source and destination', () => {
    const f = (s: string, d: string) => ({ metadata: {}, spec: { source: s, destination: d, protocol: 'TCP', verdict: 'FORWARDED' } })
    const rows = [f('a', 'b'), f('a', 'bc'), f('b', 'a')]
    expect(ff(rows, { ...EMPTY_FLOW_FILTERS, source: 'a', destination: 'b' })).toHaveLength(1)
  })
})

describe('sort parsing', () => {
  it('parses latency to ms', () => {
    expect(parseLatencyMs('12ms')).toBe(12)
    expect(parseLatencyMs('1.5s')).toBe(1500)
    expect(parseLatencyMs('500us')).toBeCloseTo(0.5)
    expect(parseLatencyMs('-')).toBeUndefined()
    expect(parseLatencyMs('')).toBeUndefined()
  })
  it('parses byte quantities', () => {
    expect(parseByteQuantity('1024')).toBe(1024)
    expect(parseByteQuantity('2KB')).toBe(2048)
    expect(parseByteQuantity('1Mi')).toBe(1048576)
    expect(parseByteQuantity('lots')).toBeUndefined()
  })
})

describe('graph focus model', () => {
  const nodes = [
    { id: 'a', label: 'web', health: 'healthy', flowCount: 3 },
    { id: 'b', label: 'db', health: 'critical', flowCount: 2 },
    { id: 'c', label: 'cache', health: 'unknown', flowCount: 0 },
  ]
  const edges = [
    { source: 'a', target: 'b', verdict: 'DROP', latency: '4ms', protocol: 'TCP' },
    { source: 'b', target: 'a', verdict: 'FORWARDED', latency: '-' },
    { source: 'c', target: 'c', verdict: 'FORWARDED' },
  ]
  it('is inactive without focus', () => {
    expect(graphFocusModel(nodes, edges, null).active).toBe(false)
  })
  it('highlights a node, its edges and neighbours', () => {
    const m = graphFocusModel(nodes, edges, { kind: 'node', id: 'a' })
    expect([...m.nodeIds].sort()).toEqual(['a', 'b'])
    expect([...m.edgeIndexes]).toEqual([0, 1])
    expect(m.readout?.heading).toBe('web')
    expect(m.readout?.rows.find((r) => r.label === 'Edges')?.value).toBe('1 outgoing, 1 incoming')
    expect(m.readout?.rows.find((r) => r.label === 'Dropped or errored edges')?.value).toBe('1')
  })
  it('reads out an edge with named endpoints', () => {
    const m = graphFocusModel(nodes, edges, { kind: 'edge', index: 1 })
    expect(m.readout?.heading).toBe('db → web')
    expect(m.readout?.rows.find((r) => r.label === 'Latency')?.value).toBe('n/a')
  })
  it('ignores unknown focus targets', () => {
    expect(graphFocusModel(nodes, edges, { kind: 'node', id: 'zzz' }).active).toBe(false)
    expect(graphFocusModel(nodes, edges, { kind: 'edge', index: 9 }).active).toBe(false)
  })
})
