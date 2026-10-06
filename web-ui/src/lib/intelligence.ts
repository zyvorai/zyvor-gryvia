import apiClient from './api'

export interface Schema {
  $ref?: string
  $defs?: Record<string, Schema>
  anyOf?: Schema[]
  type?: string
  title?: string
  description?: string
  properties?: Record<string, Schema>
  required?: string[]
  items?: Schema
  enum?: (string | number)[]
  default?: unknown
  format?: string
  minimum?: number
  maximum?: number
  exclusiveMinimum?: number
  minLength?: number
  maxLength?: number
  maxItems?: number
  minItems?: number
  pattern?: string
}
export const AREAS = [
  [
    'preflight',
    'Workload preflight',
    'Estimate memory, pool eligibility and spend before submitting.',
  ],
  [
    'training',
    'Training detective',
    'Compare framework timings and identify missing rank observations.',
  ],
  [
    'economics',
    'Useful-work economics',
    'Calculate cost per result from an explicit cost population.',
  ],
  [
    'serving',
    'Inference SLOs',
    'Check token latency and pressure; review a capacity recommendation.',
  ],
  [
    'laboratory',
    'Model laboratory',
    'Compare qualified benchmarks using quality, latency and cost.',
  ],
  [
    'recovery',
    'Recovery readiness',
    'Check a verified checkpoint against the requested recovery contract.',
  ],
  [
    'locality',
    'Dataset locality',
    'Compare verified replicas and estimated transfer time.',
  ],
  [
    'fabric',
    'Fabric qualification',
    'Evaluate supplied link tests without assuming unobserved links are healthy.',
  ],
  [
    'capacity',
    'Capacity simulator',
    'Replay demand against node shapes, rates and reservations.',
  ],
  [
    'federation',
    'Sovereign placement',
    'Filter clusters by residency, dataset availability and fresh capacity.',
  ],
] as const

export function resolved(schema: Schema, root: Schema): Schema {
  if (schema.$ref) return root.$defs?.[schema.$ref.split('/').pop() ?? ''] ?? {}
  if (schema.anyOf)
    return resolved(schema.anyOf.find((s) => s.type !== 'null') ?? {}, root)
  return schema
}

export function initial(schema: Schema, root: Schema): unknown {
  const s = resolved(schema, root)
  if (s.default !== undefined && s.default !== null)
    return structuredClone(s.default)
  if (s.enum) return s.enum[0]
  if (s.type === 'object')
    return Object.fromEntries(
      (s.required ?? []).map((k) => [
        k,
        initial(s.properties?.[k] ?? {}, root),
      ]),
    )
  if (s.type === 'array')
    return Array.from({ length: s.minItems ?? 0 }, () =>
      initial(s.items ?? {}, root),
    )
  if (s.type === 'boolean') return false
  if (s.type === 'number' || s.type === 'integer')
    return (
      s.minimum ??
      (s.exclusiveMinimum !== undefined ? s.exclusiveMinimum + 1 : 0)
    )
  return ''
}

export function label(key: string) {
  return key
    .replace(/([a-z])([A-Z])/g, '$1 $2')
    .replace(/^./, (c) => c.toUpperCase())
}

export const intelligenceApi = {
  schemas: async () =>
    (
      await apiClient.get<{ items: Record<string, Schema> }>(
        '/api/intelligence/schemas',
      )
    ).data.items,
  capabilities: async () =>
    (
      await apiClient.get<{ actionsEnabled: boolean; limitations: string[] }>(
        '/api/intelligence/capabilities',
      )
    ).data,
  analyze: async (area: string, inputs: unknown) =>
    (await apiClient.post(`/api/intelligence/${area}`, inputs)).data as Record<
      string,
      unknown
    >,
  explain: async (job: string) =>
    (
      await apiClient.get(
        `/api/intelligence/jobs/${encodeURIComponent(job)}/explain`,
      )
    ).data as Record<string, unknown>,
  economics: async (job: string) =>
    (
      await apiClient.get(
        `/api/intelligence/jobs/${encodeURIComponent(job)}/economics`,
      )
    ).data as Record<string, unknown>,
  actions: async () =>
    (
      await apiClient.get<{ items: Operation[]; truncated: boolean }>(
        '/api/intelligence/actions',
      )
    ).data,
  propose: async (proposal: unknown) =>
    (await apiClient.post('/api/intelligence/actions', proposal))
      .data as Operation,
  transition: async (id: string, transition: string) =>
    (await apiClient.post(`/api/intelligence/actions/${id}/${transition}`))
      .data as Operation,
}

export interface Operation {
  id: string
  state: string
  proposal: {
    kind: string
    name: string
    namespace: string
    value: number
    reason: string
    evidence: string
  }
  proposedBy: { issuer: string; subject: string }
  expiresAt: string
  previousValue?: unknown
}
