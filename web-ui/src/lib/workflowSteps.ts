// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Draft model behind the workflow step editor, its conversion to/from the gateway's spec JSON, and its validation.
// Gateway shape: { steps: [{ name, type, dependsOn?, jobTemplate|script|webhook, retries?, timeoutSeconds? }], parameters?: {k: v} }

import { validateDag } from './dag'
import { intError, nameError, parseIntStrict } from './forms'

export type StepType = 'job' | 'script' | 'webhook'
export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
export const HTTP_METHODS: HttpMethod[] = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE']

export type StepDraft = {
  /** Stable React key; not part of the JSON. */
  id: string
  name: string
  type: StepType
  dependsOn: string[]
  image: string
  gpus: string
  gpuType: string
  /** One line, split like a shell would (quotes group words). */
  command: string
  url: string
  method: HttpMethod
  body: string
  retries: string
  timeoutSeconds: string
}

export type ParamDraft = { id: string; key: string; value: string }

let counter = 0
export const newStepId = () => `s${++counter}`
export const newParamId = () => `w${++counter}`

export function emptyStep(patch: Partial<StepDraft> = {}): StepDraft {
  return {
    id: newStepId(),
    name: '',
    type: 'job',
    dependsOn: [],
    image: '',
    gpus: '0',
    gpuType: '',
    command: '',
    url: '',
    method: 'POST',
    body: '',
    retries: '0',
    timeoutSeconds: '0',
    ...patch,
  }
}

// --- command line <-> argv ---

/** "python train.py --name 'a b'" -> ['python', 'train.py', '--name', 'a b']. Returns null for an unterminated quote. */
export function splitCommand(line: string): string[] | null {
  const out: string[] = []
  let cur = ''
  let quote: '"' | "'" | null = null
  let inToken = false
  for (const ch of line) {
    if (quote) {
      if (ch === quote) quote = null
      else cur += ch
    } else if (ch === '"' || ch === "'") {
      quote = ch
      inToken = true
    } else if (/\s/.test(ch)) {
      if (inToken) out.push(cur)
      cur = ''
      inToken = false
    } else {
      cur += ch
      inToken = true
    }
  }
  if (quote) return null
  if (inToken) out.push(cur)
  return out
}

/** Inverse of splitCommand: quotes only the words that need it. */
export function joinCommand(args: string[]): string {
  return args
    .map((a) => {
      if (a !== '' && !/[\s"']/.test(a)) return a
      return a.includes("'") ? `"${a}"` : `'${a}'`
    })
    .join(' ')
}

// --- drafts -> gateway JSON ---

export function stepToGateway(s: StepDraft): Record<string, unknown> {
  const out: Record<string, unknown> = { name: s.name, type: s.type }
  if (s.dependsOn.length > 0) out.dependsOn = s.dependsOn
  const command = splitCommand(s.command) ?? []
  if (s.type === 'job') {
    const jt: Record<string, unknown> = { type: 'training', image: s.image.trim(), gpus: parseIntStrict(s.gpus) ?? 0 }
    if (s.gpuType.trim()) jt.gpuType = s.gpuType.trim()
    if (command.length > 0) jt.command = command
    out.jobTemplate = jt
  } else if (s.type === 'script') {
    out.script = { image: s.image.trim(), command }
  } else {
    const wh: Record<string, unknown> = { url: s.url.trim(), method: s.method }
    if (s.body !== '') wh.body = s.body
    out.webhook = wh
  }
  const retries = parseIntStrict(s.retries) ?? 0
  const timeout = parseIntStrict(s.timeoutSeconds) ?? 0
  if (retries > 0) out.retries = retries
  if (timeout > 0) out.timeoutSeconds = timeout
  return out
}

export const stepsToGateway = (steps: StepDraft[]): Record<string, unknown>[] => steps.map(stepToGateway)

export function paramsToGateway(params: ParamDraft[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const p of params) if (p.key !== '') out[p.key] = p.value
  return out
}

/** The workflow spec body: `parameters` is only included when there are any. */
export function specToGateway(steps: StepDraft[], params: ParamDraft[]): { steps: unknown[]; parameters?: Record<string, string> } {
  const parameters = paramsToGateway(params)
  return { steps: stepsToGateway(steps), ...(Object.keys(parameters).length > 0 ? { parameters } : {}) }
}

// --- gateway JSON -> drafts ---

const STEP_KEYS = new Set(['name', 'type', 'dependsOn', 'jobTemplate', 'script', 'webhook', 'retries', 'timeoutSeconds'])
const isObj = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)

function unsupported(where: string, obj: Record<string, unknown>, allowed: string[]): string | null {
  const extra = Object.keys(obj).find((k) => !allowed.includes(k))
  return extra ? `${where} has "${extra}", which the step editor cannot show; keep editing as JSON.` : null
}

export function gatewayToStep(raw: unknown): { step: StepDraft } | { error: string } {
  if (!isObj(raw)) return { error: 'Each step must be a JSON object.' }
  const bad = unsupported(`Step "${String(raw.name ?? '')}"`, raw, [...STEP_KEYS])
  if (bad) return { error: bad }
  const type = raw.type ?? 'job'
  if (type !== 'job' && type !== 'script' && type !== 'webhook') return { error: `Step "${String(raw.name ?? '')}": type must be job, script or webhook.` }
  if (raw.dependsOn !== undefined && (!Array.isArray(raw.dependsOn) || raw.dependsOn.some((d) => typeof d !== 'string'))) return { error: `Step "${String(raw.name ?? '')}": dependsOn must be a list of step names.` }
  const str = (v: unknown) => (typeof v === 'string' ? v : '')
  const num = (v: unknown) => (typeof v === 'number' ? String(v) : '0')
  const step = emptyStep({
    name: str(raw.name),
    type,
    dependsOn: (raw.dependsOn as string[] | undefined) ?? [],
    retries: num(raw.retries),
    timeoutSeconds: num(raw.timeoutSeconds),
  })
  const argv = (v: unknown) => (Array.isArray(v) ? joinCommand(v.map(String)) : '')
  if (type === 'job') {
    const jt = raw.jobTemplate
    if (!isObj(jt)) return { error: `Step "${step.name}" of type job needs a jobTemplate.` }
    const b = unsupported(`Step "${step.name}" jobTemplate`, jt, ['type', 'image', 'gpus', 'gpuType', 'command'])
    if (b) return { error: b }
    if (jt.type !== undefined && jt.type !== 'training') return { error: `Step "${step.name}": only training job templates can be shown in the step editor; keep editing as JSON.` }
    Object.assign(step, { image: str(jt.image), gpus: num(jt.gpus), gpuType: str(jt.gpuType), command: argv(jt.command) })
  } else if (type === 'script') {
    const sc = raw.script
    if (!isObj(sc)) return { error: `Step "${step.name}" of type script needs a script.` }
    const b = unsupported(`Step "${step.name}" script`, sc, ['image', 'command'])
    if (b) return { error: b }
    Object.assign(step, { image: str(sc.image), command: argv(sc.command) })
  } else {
    const wh = raw.webhook
    if (!isObj(wh)) return { error: `Step "${step.name}" of type webhook needs a webhook.` }
    const b = unsupported(`Step "${step.name}" webhook`, wh, ['url', 'method', 'body'])
    if (b) return { error: b }
    const method = wh.method ?? 'POST'
    if (!HTTP_METHODS.includes(method as HttpMethod)) return { error: `Step "${step.name}": method must be one of ${HTTP_METHODS.join(', ')}.` }
    Object.assign(step, { url: str(wh.url), method: method as HttpMethod, body: str(wh.body) })
  }
  return { step }
}

/** Parse spec JSON ({steps, parameters} or a bare steps array) into drafts; `error` says why the editor cannot show it. */
export function jsonToSpec(text: string): { steps: StepDraft[]; params: ParamDraft[] } | { error: string } {
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch (e) {
    return { error: `Not valid JSON: ${e instanceof Error ? e.message : 'parse error'}` }
  }
  const spec = Array.isArray(raw) ? { steps: raw } : raw
  if (!isObj(spec)) return { error: 'The spec must be a JSON object with a "steps" list.' }
  const extra = unsupported('The spec', spec, ['steps', 'parameters'])
  if (extra) return { error: extra }
  if (!Array.isArray(spec.steps) || spec.steps.length === 0) return { error: 'Steps must be a non-empty JSON array.' }
  if (spec.steps.length > 100) return { error: 'At most 100 steps are supported.' }
  const steps: StepDraft[] = []
  for (const s of spec.steps) {
    const r = gatewayToStep(s)
    if ('error' in r) return r
    steps.push(r.step)
  }
  const params: ParamDraft[] = []
  if (spec.parameters !== undefined) {
    if (!isObj(spec.parameters) || Object.values(spec.parameters).some((v) => typeof v !== 'string')) return { error: '"parameters" must be an object of string values.' }
    for (const [key, value] of Object.entries(spec.parameters)) params.push({ id: newParamId(), key, value: value as string })
  }
  return { steps, params }
}

// --- validation ---

export type StepErrors = Partial<Record<'name' | 'image' | 'gpus' | 'gpuType' | 'command' | 'url' | 'body' | 'retries' | 'timeoutSeconds' | 'graph', string>>

export type SpecValidation = {
  /** Aligned with the steps. */
  steps: StepErrors[]
  /** Aligned with the parameters: message for a blank or duplicate key. */
  params: (string | undefined)[]
  /** Problems with the list as a whole. */
  list: string[]
  valid: boolean
}

export function validateSpec(steps: StepDraft[], params: ParamDraft[]): SpecValidation {
  const list: string[] = []
  if (steps.length === 0) list.push('Add at least one step.')
  if (steps.length > 100) list.push('At most 100 steps are supported.')

  const errs: StepErrors[] = steps.map((s) => {
    const e: StepErrors = {}
    const n = nameError(s.name)
    if (n) e.name = n
    if (s.type === 'job' || s.type === 'script') {
      if (s.image.trim() === '') e.image = 'Enter the container image.'
      else if (/\s/.test(s.image.trim())) e.image = 'The image cannot contain spaces.'
      else if (s.image.trim().length > 512) e.image = 'Use 512 characters or fewer.'
      const argv = splitCommand(s.command)
      if (argv === null) e.command = 'Close the quotation mark.'
      else if (argv.length > 32) e.command = 'At most 32 arguments.'
      else if (s.type === 'script' && argv.length === 0) e.command = 'A script step needs a command.'
    }
    if (s.type === 'job') {
      const g = intError(s.gpus, 0, 1024)
      if (g) e.gpus = g
      if (s.gpuType.trim() !== '' && !/^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/.test(s.gpuType.trim())) e.gpuType = 'Letters, digits, . _ - only (63 characters at most).'
    }
    if (s.type === 'webhook') {
      if (!/^https?:\/\/[^\s]+$/.test(s.url.trim()) || s.url.trim().length > 2048) e.url = 'Enter an http:// or https:// URL without spaces.'
      if (s.body.length > 16384) e.body = 'The body is limited to 16384 characters.'
    }
    const r = intError(s.retries, 0, 10)
    if (r) e.retries = r
    const t = intError(s.timeoutSeconds, 0, 604800)
    if (t) e.timeoutSeconds = t
    return e
  })

  for (const issue of validateDag(steps)) {
    list.push(issue.message)
    for (const name of issue.steps) {
      steps.forEach((s, i) => {
        if (s.name === name) errs[i].graph = errs[i].graph ? `${errs[i].graph} ${issue.message}` : issue.message
      })
    }
  }

  const seen = new Map<string, number>()
  for (const p of params) seen.set(p.key, (seen.get(p.key) ?? 0) + 1)
  const paramErrs = params.map((p) => (p.key === '' ? (p.value === '' ? undefined : 'Enter a key.') : (seen.get(p.key) ?? 0) > 1 ? 'Keys must be unique.' : undefined))
  if (params.filter((p) => p.key !== '').length > 64) list.push('At most 64 parameters are supported.')

  const valid = list.length === 0 && errs.every((e) => Object.keys(e).length === 0) && paramErrs.every((e) => !e)
  return { steps: errs, params: paramErrs, list, valid }
}

// --- editor state ---

/** Steps, parameters and their JSON are kept in sync; `advanced` picks which one the user edits. */
export type SpecState = { steps: StepDraft[]; params: ParamDraft[]; json: string; advanced: boolean }

export const DEFAULT_STEPS = (): StepDraft[] => [
  emptyStep({ name: 'prepare', image: 'busybox:1.36', gpus: '0', command: 'echo prepare' }),
  emptyStep({ name: 'train', image: 'busybox:1.36', gpus: '1', command: 'echo train', dependsOn: ['prepare'] }),
]

const toJson = (steps: StepDraft[], params: ParamDraft[]) => JSON.stringify(specToGateway(steps, params), null, 2)

export function initialSpecState(steps: StepDraft[] = DEFAULT_STEPS(), params: ParamDraft[] = []): SpecState {
  return { steps, params, json: toJson(steps, params), advanced: false }
}

export function withSpec(state: SpecState, steps: StepDraft[], params: ParamDraft[]): SpecState {
  return { ...state, steps, params, json: toJson(steps, params) }
}

export function withSpecJson(state: SpecState, json: string): SpecState {
  const parsed = jsonToSpec(json)
  return 'error' in parsed ? { ...state, json } : { ...state, json, steps: parsed.steps, params: parsed.params }
}

/** First problem with the spec as it will be submitted (null when valid). */
export function specError(state: SpecState): string | null {
  if (!state.advanced) {
    const v = validateSpec(state.steps, state.params)
    return v.valid ? null : (v.list[0] ?? 'Fix the highlighted fields.')
  }
  const parsed = jsonToSpec(state.json)
  if ('error' in parsed) {
    // JSON the editor cannot show may still be a valid spec (extra fields); check what we can, the gateway does the rest.
    return rawSpecError(state.json) ?? null
  }
  const v = validateSpec(parsed.steps, parsed.params)
  return v.valid ? null : (v.list[0] ?? 'Fix the highlighted fields.')
}

function rawSpecError(text: string): string | null {
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch (e) {
    return `Not valid JSON: ${e instanceof Error ? e.message : 'parse error'}`
  }
  const steps = Array.isArray(raw) ? raw : isObj(raw) ? raw.steps : undefined
  if (!Array.isArray(steps) || steps.length === 0) return 'Steps must be a non-empty JSON array.'
  if (!steps.every((s) => isObj(s) && typeof s.name === 'string')) return 'Each step needs a name.'
  const nodes = (steps as Record<string, unknown>[]).map((s) => ({ name: s.name as string, dependsOn: Array.isArray(s.dependsOn) ? (s.dependsOn as string[]) : [] }))
  const bad = nodes.map((s) => nameError(s.name)).find(Boolean)
  if (bad) return bad
  return validateDag(nodes)[0]?.message ?? null
}

/** The spec body sent to the gateway (from the JSON when it is being edited by hand). */
export function specBody(state: SpecState): { steps: unknown[]; parameters?: Record<string, string> } {
  if (!state.advanced) return specToGateway(state.steps, state.params)
  const raw = JSON.parse(state.json) as unknown
  return Array.isArray(raw) ? { steps: raw } : (raw as { steps: unknown[]; parameters?: Record<string, string> })
}
