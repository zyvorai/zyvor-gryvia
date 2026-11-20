// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Row model behind the tuner's parameter-space editor, and its conversion to/from the JSON the gateway accepts:
// { "<name>": { type: "float"|"int", min, max, scale?, step? } | { type: "choice", values: [...] } }

import { validateParameterSpace } from './tuner'

export type ParamType = 'float' | 'int' | 'choice'

export type ParamRow = {
  /** Stable React key; not part of the JSON. */
  id: string
  name: string
  type: ParamType
  min: string
  max: string
  scale: 'linear' | 'log'
  /** Optional; blank means unset. */
  step: string
  values: string[]
}

export type RowErrors = Partial<Record<'name' | 'min' | 'max' | 'step' | 'values', string>>

export const PARAM_NAME = /^[A-Za-z_][A-Za-z0-9_.-]{0,62}$/
export const MAX_PARAMS = 32

let counter = 0
export const newRowId = () => `p${++counter}`

export function emptyRow(patch: Partial<ParamRow> = {}): ParamRow {
  return { id: newRowId(), name: '', type: 'float', min: '', max: '', scale: 'linear', step: '', values: [], ...patch }
}

const numberOrUndefined = (raw: string): number | undefined => {
  const t = raw.trim()
  if (t === '') return undefined
  const n = Number(t)
  return Number.isFinite(n) ? n : undefined
}

/** Chip text -> JSON scalar: "16" -> 16, "true" -> true, anything else stays a string. */
export function parseChipValue(v: string): string | number | boolean {
  const t = v.trim()
  if (t === 'true') return true
  if (t === 'false') return false
  if (t !== '' && Number.isFinite(Number(t)) && /^-?(\d+\.?\d*|\.\d+)(e[-+]?\d+)?$/i.test(t)) return Number(t)
  return v
}

/** Rows -> the gateway's parameter-space object. Unfinished numeric fields are left out (the validators flag them). */
export function rowsToSpace(rows: ParamRow[]): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const r of rows) {
    if (r.type === 'choice') {
      out[r.name] = { type: 'choice', values: r.values.map(parseChipValue) }
      continue
    }
    const spec: Record<string, unknown> = { type: r.type }
    const min = numberOrUndefined(r.min)
    const max = numberOrUndefined(r.max)
    if (min !== undefined) spec.min = min
    if (max !== undefined) spec.max = max
    if (r.scale === 'log') spec.scale = 'log'
    const step = numberOrUndefined(r.step)
    if (step !== undefined) spec.step = step
    out[r.name] = spec
  }
  return out
}

export const rowsToJson = (rows: ParamRow[]): string => JSON.stringify(rowsToSpace(rows), null, 2)

/** Parse gateway-shaped JSON into rows; `error` says why the editor cannot show it (the JSON can still be edited). */
export function jsonToRows(text: string): { rows: ParamRow[] } | { error: string } {
  const error = validateParameterSpace(text)
  if (error) return { error }
  const raw = JSON.parse(text) as Record<string, Record<string, unknown>>
  const rows = Object.entries(raw).map(([name, p]) => {
    if (p.type === 'choice') return emptyRow({ name, type: 'choice', values: (p.values as unknown[]).map((v) => String(v)) })
    return emptyRow({
      name,
      type: p.type as 'float' | 'int',
      min: String(p.min),
      max: String(p.max),
      scale: p.scale === 'log' ? 'log' : 'linear',
      step: p.step === undefined ? '' : String(p.step),
    })
  })
  return { rows }
}

/** Per-row validation (aligned with `rows`). */
export function validateRows(rows: ParamRow[]): RowErrors[] {
  const counts = new Map<string, number>()
  for (const r of rows) counts.set(r.name, (counts.get(r.name) ?? 0) + 1)
  return rows.map((r) => {
    const e: RowErrors = {}
    if (r.name === '') e.name = 'Enter a name.'
    else if (!PARAM_NAME.test(r.name)) e.name = 'Start with a letter or _; then letters, digits, _ . - (63 characters at most).'
    else if ((counts.get(r.name) ?? 0) > 1) e.name = 'Names must be unique.'

    if (r.type === 'choice') {
      if (r.values.length < 1) e.values = 'Add at least one value.'
      else if (r.values.length > 64) e.values = 'At most 64 values.'
      else if (r.values.some((v) => v.length > 128)) e.values = 'Each value must be 128 characters or fewer.'
      return e
    }

    const min = numberOrUndefined(r.min)
    const max = numberOrUndefined(r.max)
    if (min === undefined) e.min = 'Enter a number.'
    if (max === undefined) e.max = 'Enter a number.'
    if (min !== undefined && max !== undefined && min >= max) e.max = 'Must be greater than min.'
    if (min !== undefined && r.scale === 'log' && min <= 0) e.min = 'Log scale needs a min above 0.'
    if (r.step.trim() !== '') {
      const step = numberOrUndefined(r.step)
      if (step === undefined || !Number.isInteger(step) || step < 1) e.step = 'A positive whole number, or leave blank.'
    }
    return e
  })
}

/** The editor's whole state: rows and JSON are kept in sync; `advanced` picks which one the user is editing. */
export type ParamSpaceState = { rows: ParamRow[]; json: string; advanced: boolean }

export const DEFAULT_SPACE_ROWS = (): ParamRow[] => [
  emptyRow({ name: 'learning_rate', type: 'float', min: '0.00001', max: '0.01', scale: 'log' }),
  emptyRow({ name: 'batch_size', type: 'choice', values: ['16', '32', '64', '128'] }),
  emptyRow({ name: 'dropout', type: 'float', min: '0', max: '0.5' }),
]

export function initialSpaceState(rows: ParamRow[] = DEFAULT_SPACE_ROWS()): ParamSpaceState {
  return { rows, json: rowsToJson(rows), advanced: false }
}

export function withRows(state: ParamSpaceState, rows: ParamRow[]): ParamSpaceState {
  return { ...state, rows, json: rowsToJson(rows) }
}

/** JSON edited by hand: rows follow when the text is understood, otherwise they keep their last good value. */
export function withJson(state: ParamSpaceState, json: string): ParamSpaceState {
  const parsed = jsonToRows(json)
  return { ...state, json, rows: 'rows' in parsed ? parsed.rows : state.rows }
}

/** First problem with the space as it will be submitted (null when valid). */
export function spaceError(state: ParamSpaceState): string | null {
  if (state.advanced) return validateParameterSpace(state.json)
  if (state.rows.length === 0) return 'Add at least one parameter.'
  if (state.rows.length > MAX_PARAMS) return `At most ${MAX_PARAMS} parameters are supported.`
  const first = validateRows(state.rows).findIndex((e) => Object.keys(e).length > 0)
  return first >= 0 ? `Fix parameter ${first + 1}.` : null
}

/** The JSON string sent to the gateway. */
export const spaceJson = (state: ParamSpaceState): string => (state.advanced ? state.json : rowsToJson(state.rows))
