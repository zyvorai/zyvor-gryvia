// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useEffect, useId, useRef, useState } from 'react'
import { emptyRow, jsonToRows, MAX_PARAMS, rowsToJson, spaceError, validateRows, withJson, withRows, type ParamRow, type ParamSpaceState, type RowErrors } from '@/lib/paramSpace'

type Props = {
  state: ParamSpaceState
  onChange: (next: ParamSpaceState) => void
  /** Show every row's errors (after a submit attempt), not only rows already edited. */
  showErrors?: boolean
}

/** Structured editor for the tuner's parameter space, with an "Advanced" JSON view kept in sync. */
export default function ParamSpaceEditor({ state, onChange, showErrors = false }: Props) {
  const uid = useId()
  const [touched, setTouched] = useState<Set<string>>(() => new Set())
  const focusRow = useRef<string | null>(null)
  const errors = validateRows(state.rows)
  const jsonProblem = state.advanced ? spaceError(state) : null
  const canLeaveJson = !state.advanced || 'rows' in jsonToRows(state.json)

  useEffect(() => {
    if (!focusRow.current) return
    document.getElementById(`${uid}-${focusRow.current}-name`)?.focus()
    focusRow.current = null
  })

  const edit = (id: string, patch: Partial<ParamRow>) => {
    setTouched((t) => new Set(t).add(id))
    onChange(withRows(state, state.rows.map((r) => (r.id === id ? { ...r, ...patch } : r))))
  }
  const add = () => {
    const row = emptyRow()
    focusRow.current = row.id
    onChange(withRows(state, [...state.rows, row]))
  }
  const remove = (id: string) => onChange(withRows(state, state.rows.filter((r) => r.id !== id)))

  const toggleAdvanced = () => {
    if (state.advanced) {
      const parsed = jsonToRows(state.json)
      if ('rows' in parsed) onChange({ rows: parsed.rows, json: rowsToJson(parsed.rows), advanced: false })
    } else {
      onChange({ ...state, json: rowsToJson(state.rows), advanced: true })
    }
  }

  return (
    <div className="stack">
      <div className="row">
        <p className="eyebrow">Parameter space</p>
        <button type="button" className="btn-secondary" aria-pressed={state.advanced} onClick={toggleAdvanced} disabled={!canLeaveJson}>
          {state.advanced ? 'Back to the row editor' : 'Advanced: edit as JSON'}
        </button>
      </div>

      {state.advanced ? (
        <label className="field">
          Parameter space (JSON)
          <textarea
            className="codeedit"
            rows={8}
            value={state.json}
            onChange={(e) => onChange(withJson(state, e.target.value))}
            aria-invalid={!!jsonProblem}
            aria-describedby={`${uid}-json-msg`}
            spellCheck={false}
          />
          <span id={`${uid}-json-msg`} className={jsonProblem ? 'warning' : 'faint'} role={jsonProblem ? 'alert' : undefined}>
            {jsonProblem ?? 'One entry per parameter: float and int take min and max (optional scale "log", integer step); choice takes a values list.'}
          </span>
        </label>
      ) : (
        <>
          {state.rows.length === 0 && <p className="faint">No parameters yet. Add at least one for the tuner to search over.</p>}
          {state.rows.map((row, i) => (
            <ParamRowFields
              key={row.id}
              uid={`${uid}-${row.id}`}
              index={i}
              row={row}
              errors={showErrors || touched.has(row.id) ? errors[i] : {}}
              onEdit={(patch) => edit(row.id, patch)}
              onRemove={() => remove(row.id)}
            />
          ))}
          <div className="toolbar">
            <button type="button" className="btn-secondary" onClick={add} disabled={state.rows.length >= MAX_PARAMS}>
              Add parameter
            </button>
            {showErrors && state.rows.length === 0 && (
              <span className="warning" role="alert">
                Add at least one parameter.
              </span>
            )}
          </div>
        </>
      )}
    </div>
  )
}

function ParamRowFields({ uid, index, row, errors, onEdit, onRemove }: { uid: string; index: number; row: ParamRow; errors: RowErrors; onEdit: (patch: Partial<ParamRow>) => void; onRemove: () => void }) {
  const msg = (key: keyof RowErrors, hint?: string) => {
    const text = errors[key] ?? hint
    return text ? (
      <span id={`${uid}-${key}-msg`} className={errors[key] ? 'warning' : 'faint'}>
        {text}
      </span>
    ) : null
  }
  const describe = (key: keyof RowErrors, hint?: boolean) => (errors[key] || hint ? `${uid}-${key}-msg` : undefined)
  const numeric = row.type !== 'choice'

  return (
    <fieldset className="card">
      <legend className="eyebrow">Parameter {index + 1}{row.name ? `: ${row.name}` : ''}</legend>
      <div className="formgrid">
        <label className="field">
          Name
          <input id={`${uid}-name`} type="text" value={row.name} onChange={(e) => onEdit({ name: e.target.value })} placeholder="learning_rate" autoComplete="off" spellCheck={false} aria-invalid={!!errors.name} aria-describedby={describe('name')} />
          {msg('name')}
        </label>
        <label className="field">
          Type
          <select value={row.type} onChange={(e) => onEdit({ type: e.target.value as ParamRow['type'] })}>
            <option value="float">float</option>
            <option value="int">int</option>
            <option value="choice">choice</option>
          </select>
        </label>
        {numeric && (
          <>
            <label className="field">
              Min
              <input type="text" inputMode="decimal" value={row.min} onChange={(e) => onEdit({ min: e.target.value })} placeholder="0.0001" aria-invalid={!!errors.min} aria-describedby={describe('min')} />
              {msg('min')}
            </label>
            <label className="field">
              Max
              <input type="text" inputMode="decimal" value={row.max} onChange={(e) => onEdit({ max: e.target.value })} placeholder="0.1" aria-invalid={!!errors.max} aria-describedby={describe('max')} />
              {msg('max')}
            </label>
            <label className="field">
              Scale
              <select value={row.scale} onChange={(e) => onEdit({ scale: e.target.value as ParamRow['scale'] })}>
                <option value="linear">linear</option>
                <option value="log">log</option>
              </select>
            </label>
            <label className="field">
              Step (optional)
              <input type="text" inputMode="numeric" value={row.step} onChange={(e) => onEdit({ step: e.target.value })} aria-invalid={!!errors.step} aria-describedby={describe('step')} />
              {msg('step')}
            </label>
          </>
        )}
      </div>
      {!numeric && (
        <ChipsInput
          uid={uid}
          label="Values"
          values={row.values}
          onChange={(values) => onEdit({ values })}
          error={errors.values}
          hint="Type a value and press Enter or comma. Up to 64."
        />
      )}
      <div className="toolbar">
        <button type="button" className="danger" onClick={onRemove} aria-label={`Remove parameter ${index + 1}${row.name ? ` ${row.name}` : ''}`}>
          Remove
        </button>
      </div>
    </fieldset>
  )
}

function ChipsInput({ uid, label, values, onChange, error, hint }: { uid: string; label: string; values: string[]; onChange: (v: string[]) => void; error?: string; hint: string }) {
  const [draft, setDraft] = useState('')
  const commit = (raw: string) => {
    const parts = raw
      .split(',')
      .map((p) => p.trim())
      .filter((p) => p !== '' && !values.includes(p))
    if (parts.length > 0) onChange([...values, ...parts])
    setDraft('')
  }
  const msgId = `${uid}-values-msg`
  return (
    <div className="field">
      <label htmlFor={`${uid}-values`}>{label}</label>
      {values.length > 0 && (
        <ul className="row" aria-label={`${label} added`}>
          {values.map((v) => (
            <li key={v} className="pill">
              <span className="mono">{v}</span>{' '}
              <button type="button" className="th-sort" aria-label={`Remove value ${v}`} onClick={() => onChange(values.filter((x) => x !== v))}>
                <span aria-hidden="true">×</span>
              </button>
            </li>
          ))}
        </ul>
      )}
      <input
        id={`${uid}-values`}
        type="text"
        value={draft}
        placeholder="16"
        autoComplete="off"
        aria-invalid={!!error}
        aria-describedby={msgId}
        onChange={(e) => (e.target.value.includes(',') ? commit(e.target.value) : setDraft(e.target.value))}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            // Enter adds the value; it must not submit the surrounding form.
            e.preventDefault()
            commit(draft)
          } else if (e.key === 'Backspace' && draft === '' && values.length > 0) {
            onChange(values.slice(0, -1))
          }
        }}
        onBlur={() => commit(draft)}
      />
      <span id={msgId} className={error ? 'warning' : 'faint'}>
        {error ?? hint}
      </span>
    </div>
  )
}
