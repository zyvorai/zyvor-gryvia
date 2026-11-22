// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Pure helpers for the network pages (graph geometry, verdicts, flow filters, policy buckets, form validation).
import type { Tone } from './phase'

export type VerdictKind = 'good' | 'drop' | 'error' | 'unknown'

/** FORWARDED/ALLOW are fine, DROP/DENIED are policy drops, ERROR is a measurement/plumbing problem. */
export function verdictKind(verdict?: string): VerdictKind {
  switch ((verdict ?? '').trim().toUpperCase()) {
    case 'FORWARDED':
    case 'ALLOW':
      return 'good'
    case 'DROP':
    case 'DENIED':
      return 'drop'
    case 'ERROR':
      return 'error'
    default:
      return 'unknown'
  }
}

/** Pill tone for a verdict: forwarded is neutral (colour = deviation), drops red, errors amber. */
export function verdictTone(verdict?: string): Tone {
  const k = verdictKind(verdict)
  return k === 'drop' ? 'bad' : k === 'error' ? 'warn' : ''
}

export type HealthKind = 'healthy' | 'warning' | 'critical' | 'unknown'
export function healthKind(health?: string): HealthKind {
  const h = (health ?? '').trim().toLowerCase()
  return h === 'healthy' || h === 'warning' || h === 'critical' ? h : 'unknown'
}

/** "l7" -> "L7", "l3_l4" / "l3,l4" -> "L3/L4". */
export function formatCaptureLevel(level?: string): string {
  if (!level) return '—'
  return level
    .trim()
    .toUpperCase()
    .split(/[\s,_/+-]+/)
    .filter(Boolean)
    .join('/')
}

export type Pt = { x: number; y: number }

/**
 * Line between two node boxes, clipped to the box borders and shifted sideways by `offset`
 * (perpendicular to the direction of travel). A->B and B->A both shift to their own left, so a
 * two-way pair renders as two parallel lines `2 * offset` apart instead of overlapping.
 */
export function edgeGeometry(from: Pt, to: Pt, boxW: number, boxH: number, offset: number, gap = 6): { start: Pt; end: Pt; mid: Pt } {
  const dx = to.x - from.x
  const dy = to.y - from.y
  const dist = Math.hypot(dx, dy) || 1
  const nx = dx / dist
  const ny = dy / dist
  const px = -ny * offset
  const py = nx * offset
  // Distance from centre to the box border along the direction of travel.
  const t = Math.min(nx === 0 ? Infinity : boxW / 2 / Math.abs(nx), ny === 0 ? Infinity : boxH / 2 / Math.abs(ny))
  const start = { x: from.x + nx * t + px, y: from.y + ny * t + py }
  const end = { x: to.x - nx * (t + gap) + px, y: to.y - ny * (t + gap) + py }
  return { start, end, mid: { x: (start.x + end.x) / 2, y: (start.y + end.y) / 2 } }
}

/** Which edges have a reverse edge in the list (A->B and B->A). */
export function twoWayEdges(edges: Array<{ source: string; target: string }>): boolean[] {
  const seen = new Set(edges.map((e) => `${e.source}\u0000${e.target}`))
  return edges.map((e) => e.source !== e.target && seen.has(`${e.target}\u0000${e.source}`))
}

// ---- flows -------------------------------------------------------------------------------

export type FlowLike = { metadata: { namespace?: string }; spec: { source: string; destination: string; protocol: string; verdict: string } }
/** `source` / `destination` are exact matches (used by graph edge links); `service` matches either end by substring. */
export type FlowFilters = { service: string; namespace: string; verdict: string; protocol: string; source?: string; destination?: string }
export const EMPTY_FLOW_FILTERS: FlowFilters = { service: '', namespace: '', verdict: '', protocol: '', source: '', destination: '' }

const ci = (s: string | undefined) => (s ?? '').toLowerCase()

export function filterFlows<T extends FlowLike>(flows: T[], f: FlowFilters): T[] {
  const svc = ci(f.service).trim()
  const ns = ci(f.namespace).trim()
  const src = ci(f.source).trim()
  const dst = ci(f.destination).trim()
  return flows.filter((x) => {
    if (src && ci(x.spec.source) !== src) return false
    if (dst && ci(x.spec.destination) !== dst) return false
    if (svc && !ci(x.spec.source).includes(svc) && !ci(x.spec.destination).includes(svc)) return false
    if (ns && ci(x.metadata.namespace) !== ns) return false
    if (f.verdict && ci(x.spec.verdict) !== ci(f.verdict)) return false
    if (f.protocol && ci(x.spec.protocol) !== ci(f.protocol)) return false
    return true
  })
}

/** Distinct, sorted, non-empty values (case-insensitive de-duplication, first spelling wins). */
export function distinctValues(values: Array<string | undefined>): string[] {
  const seen = new Map<string, string>()
  for (const v of values) {
    const t = (v ?? '').trim()
    if (t && !seen.has(t.toLowerCase())) seen.set(t.toLowerCase(), t)
  }
  return [...seen.values()].sort((a, b) => a.localeCompare(b))
}

export function flowFilterOptions(flows: FlowLike[]): { protocols: string[]; verdicts: string[]; namespaces: string[] } {
  return {
    protocols: distinctValues(flows.map((f) => f.spec.protocol)),
    verdicts: distinctValues(flows.map((f) => f.spec.verdict)),
    namespaces: distinctValues(flows.map((f) => f.metadata.namespace)),
  }
}

/** Page numbers to show: a window of up to `size` around `page`, always inside 1..total. */
export function pageWindow(page: number, total: number, size = 5): number[] {
  const n = Math.min(size, total)
  let start = page - Math.floor(n / 2)
  start = Math.max(1, Math.min(start, total - n + 1))
  return Array.from({ length: n }, (_, i) => start + i)
}

// ---- policies ----------------------------------------------------------------------------

/** No status = Pending. Enforced/Active enforced; Suggested; anything else lands in "other" so nothing vanishes. */
export type PolicyBucket = 'enforced' | 'pending' | 'suggested' | 'other'
export function policyBucket(phase?: string): PolicyBucket {
  const p = (phase ?? '').trim().toLowerCase()
  if (!p || p === 'pending') return 'pending'
  if (p === 'enforced' || p === 'active') return 'enforced'
  if (p === 'suggested') return 'suggested'
  return 'other'
}

export function bucketPolicies<T>(items: T[] | undefined, phaseOf: (t: T) => string | undefined): Record<PolicyBucket, T[]> {
  const out: Record<PolicyBucket, T[]> = { enforced: [], pending: [], suggested: [], other: [] }
  for (const it of items ?? []) out[policyBucket(phaseOf(it))].push(it)
  return out
}

export type PolicyFormValues = { name: string; sourceService: string; destinationService: string; port: string; intent: string }
export type PolicyFormErrors = Partial<Record<keyof PolicyFormValues, string>>

export const K8S_NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
export const INTENT_MAX = 200

export function validateK8sName(v: string, label: string): string | undefined {
  if (!v) return `${label} is required.`
  if (v.length > 63) return `${label} must be 63 characters or fewer.`
  if (!K8S_NAME.test(v)) return `${label} must be lowercase letters, digits or "-", starting and ending with a letter or digit.`
  return undefined
}

export function validatePolicyForm(f: PolicyFormValues): PolicyFormErrors {
  const e: PolicyFormErrors = {}
  const name = f.name.trim()
  if (name) {
    const n = validateK8sName(name, 'Name')
    if (n) e.name = n
  }
  const s = validateK8sName(f.sourceService.trim(), 'Source')
  if (s) e.sourceService = s
  const d = validateK8sName(f.destinationService.trim(), 'Destination')
  if (d) e.destinationService = d
  const port = Number(f.port)
  if (f.port.trim() === '' || !Number.isInteger(port) || port < 1 || port > 65535) e.port = 'Port must be a whole number from 1 to 65535.'
  const intent = f.intent.trim()
  if (!intent) e.intent = 'Intent is required.'
  else if (intent.length > INTENT_MAX) e.intent = `Intent must be ${INTENT_MAX} characters or fewer.`
  return e
}

/** One severity vocabulary for network anomalies and security alerts. */
export function severityTone(severity?: string): Tone {
  switch ((severity ?? '').toLowerCase()) {
    case 'critical':
      return 'bad'
    case 'high':
      return 'warn'
    case 'medium':
      return 'info'
    default:
      return ''
  }
}

// ---- navigation between pages (URL contracts) ---------------------------------------------

export const POLICY_PROTOCOLS = ['TCP', 'UDP', 'HTTP', 'gRPC'] as const

const query = (o: Record<string, string>) => new URLSearchParams(o).toString()

/** Flows page filtered to one service (either end). */
export const flowsUrlForService = (name: string) => `/network/flows?${query({ service: name })}`
/** Flows page filtered to one directed connection. */
export const flowsUrlForEdge = (source: string, destination: string) => `/network/flows?${query({ source, destination })}`
/** Overview with the "Create trace" dialog opened for a target. */
export const traceUrl = (target: string) => `/network?${query({ trace: target })}`
export function policyPrefillUrl(c: { source: string; destination: string; port: number | string; protocol: string }): string {
  return `/network/policies?${query({ new: '1', src: c.source, dst: c.destination, port: String(c.port), protocol: c.protocol })}`
}

export type PolicyPrefill = { open: boolean; sourceService: string; destinationService: string; port: string; protocol: string }

/** `?new=1&src&dst&port&protocol` -> initial values for the create-policy form. Junk falls back to safe defaults. */
export function parsePolicyPrefill(params: URLSearchParams): PolicyPrefill {
  const port = (params.get('port') ?? '').trim()
  const proto = (params.get('protocol') ?? '').trim().toLowerCase()
  return {
    open: params.get('new') === '1',
    sourceService: (params.get('src') ?? '').trim(),
    destinationService: (params.get('dst') ?? '').trim(),
    port: /^\d{1,5}$/.test(port) ? port : '80',
    protocol: POLICY_PROTOCOLS.find((p) => p.toLowerCase() === proto) ?? 'TCP',
  }
}

/** `?trace=<service>` -> the target to prefill (null when absent). */
export function parseTraceTarget(params: URLSearchParams): string | null {
  const t = (params.get('trace') ?? '').trim()
  return t || null
}

// ---- flow sorting helpers ----------------------------------------------------------------

/** "12ms" / "1.5s" / "800us" -> milliseconds; undefined for blank, "-" or unparseable. */
export function parseLatencyMs(latency?: string): number | undefined {
  const m = /^(\d+(?:\.\d+)?)\s*(µs|us|ms|s|m)?$/i.exec((latency ?? '').trim())
  if (!m) return undefined
  const n = Number(m[1])
  switch ((m[2] ?? 'ms').toLowerCase()) {
    case 'µs':
    case 'us':
      return n / 1000
    case 's':
      return n * 1000
    case 'm':
      return n * 60000
    default:
      return n
  }
}

/** "1024" / "1.5KB" / "2Mi" / "3 GB" -> bytes; undefined when unparseable. */
export function parseByteQuantity(v?: string | number | null): number | undefined {
  if (typeof v === 'number') return Number.isFinite(v) ? v : undefined
  const m = /^(\d+(?:\.\d+)?)\s*([kmgt])?i?b?$/i.exec((v ?? '').trim())
  if (!m) return undefined
  const pow = m[2] ? 'kmgt'.indexOf(m[2].toLowerCase()) + 1 : 0
  return Number(m[1]) * 1024 ** pow
}

// ---- service graph focus model -------------------------------------------------------------

export type GraphNodeLike = { id: string; label: string; health?: string; flowCount: number }
export type GraphEdgeLike = { source: string; target: string; protocol?: string; latency?: string; verdict?: string }
export type GraphFocus = { kind: 'node'; id: string } | { kind: 'edge'; index: number } | null
export type GraphReadout = { heading: string; rows: Array<{ label: string; value: string }> }
export type GraphFocusModel = { active: boolean; nodeIds: Set<string>; edgeIndexes: Set<number>; readout: GraphReadout | null }

const latencyText = (l?: string) => (l && l.trim() && l.trim() !== '-' ? l.trim() : 'n/a')

/** What is highlighted (the focus, its neighbours, its edges) and what the detail card reads out. */
export function graphFocusModel(nodes: GraphNodeLike[], edges: GraphEdgeLike[], focus: GraphFocus): GraphFocusModel {
  const byId = new Map(nodes.map((n) => [n.id, n]))
  const name = (id: string) => byId.get(id)?.label ?? id
  const nodeIds = new Set<string>()
  const edgeIndexes = new Set<number>()
  if (!focus) return { active: false, nodeIds, edgeIndexes, readout: null }

  if (focus.kind === 'edge') {
    const e = edges[focus.index]
    if (!e) return { active: false, nodeIds, edgeIndexes, readout: null }
    edgeIndexes.add(focus.index)
    nodeIds.add(e.source)
    nodeIds.add(e.target)
    return {
      active: true,
      nodeIds,
      edgeIndexes,
      readout: {
        heading: `${name(e.source)} → ${name(e.target)}`,
        rows: [
          { label: 'Verdict', value: e.verdict?.trim() || 'Unknown' },
          { label: 'Latency', value: latencyText(e.latency) },
          { label: 'Protocol', value: e.protocol?.trim() || 'n/a' },
        ],
      },
    }
  }

  const node = byId.get(focus.id)
  if (!node) return { active: false, nodeIds, edgeIndexes, readout: null }
  nodeIds.add(node.id)
  let outgoing = 0
  let incoming = 0
  let problems = 0
  edges.forEach((e, i) => {
    if (e.source !== node.id && e.target !== node.id) return
    edgeIndexes.add(i)
    nodeIds.add(e.source)
    nodeIds.add(e.target)
    if (e.source === node.id) outgoing += 1
    else incoming += 1
    const k = verdictKind(e.verdict)
    if (k === 'drop' || k === 'error') problems += 1
  })
  return {
    active: true,
    nodeIds,
    edgeIndexes,
    readout: {
      heading: node.label,
      rows: [
        { label: 'Health', value: healthKind(node.health)[0].toUpperCase() + healthKind(node.health).slice(1) },
        { label: 'Connections', value: String(node.flowCount) },
        { label: 'Edges', value: `${outgoing} outgoing, ${incoming} incoming` },
        { label: 'Dropped or errored edges', value: String(problems) },
      ],
    },
  }
}

// ---- trace form ----------------------------------------------------------------------------

export const TRACE_LEVELS: Array<{ value: 'l3' | 'l4' | 'l7'; label: string; help: string }> = [
  { value: 'l3', label: 'L3', help: 'IP-level metadata only. Lowest overhead and cost.' },
  { value: 'l4', label: 'L4', help: 'Adds TCP/UDP ports and connection state. Moderate overhead.' },
  { value: 'l7', label: 'L7', help: 'Parses HTTP/gRPC requests. Highest CPU overhead and capture volume; keep durations short.' },
]
export const DURATION_UNITS = ['s', 'm', 'h'] as const
export const DURATION_PATTERN = /^[1-9][0-9]{0,3}[smh]$/

export type TraceFormValues = { targetService: string; namespace: string; captureLevel: string; durationAmount: string; durationUnit: string }
export type TraceFormErrors = Partial<Record<keyof TraceFormValues, string>>

/** "30" + "m" -> "30m"; null when the gateway would reject it. */
export function buildTraceDuration(amount: string, unit: string): string | null {
  const v = `${amount.trim()}${unit}`
  return DURATION_PATTERN.test(v) ? v : null
}

export function validateTraceForm(f: TraceFormValues): TraceFormErrors {
  const e: TraceFormErrors = {}
  const t = validateK8sName(f.targetService.trim(), 'Target service')
  if (t) e.targetService = t
  const n = validateK8sName(f.namespace.trim(), 'Namespace')
  if (n) e.namespace = n
  if (!TRACE_LEVELS.some((l) => l.value === f.captureLevel)) e.captureLevel = 'Choose L3, L4 or L7.'
  if (!(DURATION_UNITS as readonly string[]).includes(f.durationUnit)) e.durationUnit = 'Choose seconds, minutes or hours.'
  else if (!buildTraceDuration(f.durationAmount, f.durationUnit)) e.durationAmount = 'Enter a whole number from 1 to 9999 with no leading zero.'
  return e
}
