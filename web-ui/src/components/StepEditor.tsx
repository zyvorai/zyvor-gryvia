// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useEffect, useId, useRef, useState } from 'react'
import { dagLayers, describeSteps } from '@/lib/dag'
import {
  emptyStep,
  HTTP_METHODS,
  jsonToSpec,
  newParamId,
  specError,
  validateSpec,
  withSpec,
  withSpecJson,
  type HttpMethod,
  type SpecState,
  type StepDraft,
  type StepErrors,
  type StepType,
} from '@/lib/workflowSteps'

type Props = {
  state: SpecState
  onChange: (next: SpecState) => void
  /** Show every step's errors (after a submit attempt), not only steps already edited. */
  showErrors?: boolean
}

/** Ordered step-list editor with live DAG validation, a layered preview and an "Advanced" JSON view kept in sync. */
export default function StepEditor({ state, onChange, showErrors = false }: Props) {
  const uid = useId()
  const [touched, setTouched] = useState<Set<string>>(() => new Set())
  const pendingFocus = useRef<string | null>(null)
  const validation = validateSpec(state.steps, state.params)
  const jsonProblem = state.advanced ? specError(state) : null
  const canLeaveJson = !state.advanced || !('error' in jsonToSpec(state.json))

  useEffect(() => {
    if (!pendingFocus.current) return
    document.getElementById(pendingFocus.current)?.focus()
    pendingFocus.current = null
  })

  const touch = (id: string) => setTouched((t) => (t.has(id) ? t : new Set(t).add(id)))
  const setSteps = (steps: StepDraft[]) => onChange(withSpec(state, steps, state.params))

  // The last non-blank name each step had, so clearing a name field and retyping keeps its dependents attached.
  const lastName = useRef(new Map<string, string>())
  const nameBefore = (step: StepDraft) => (step.name !== '' ? step.name : lastName.current.get(step.id) ?? '')

  const edit = (id: string, patch: Partial<StepDraft>) => {
    touch(id)
    const old = state.steps.find((s) => s.id === id)
    let steps = state.steps.map((s) => (s.id === id ? { ...s, ...patch } : s))
    if (old && patch.name !== undefined) {
      const prev = nameBefore(old)
      // Renaming a step keeps everything that depended on it pointing at it (a blank name waits for the next one).
      if (patch.name !== '' && prev !== '' && prev !== patch.name) {
        steps = steps.map((s) => ({ ...s, dependsOn: s.dependsOn.map((d) => (d === prev ? (patch.name as string) : d)) }))
      }
      if (patch.name !== '') lastName.current.set(id, patch.name)
      else if (prev !== '') lastName.current.set(id, prev)
    }
    setSteps(steps)
  }
  const add = () => {
    const step = emptyStep({ name: uniqueName(state.steps, 'step') })
    pendingFocus.current = `${uid}-${step.id}-name`
    setSteps([...state.steps, step])
  }
  const remove = (id: string) => {
    const gone = state.steps.find((s) => s.id === id)
    const goneName = gone ? nameBefore(gone) : ''
    lastName.current.delete(id)
    setSteps(state.steps.filter((s) => s.id !== id).map((s) => ({ ...s, dependsOn: s.dependsOn.filter((d) => d !== goneName) })))
  }
  const move = (id: string, dir: -1 | 1) => {
    const i = state.steps.findIndex((s) => s.id === id)
    const j = i + dir
    if (i < 0 || j < 0 || j >= state.steps.length) return
    const steps = [...state.steps]
    ;[steps[i], steps[j]] = [steps[j], steps[i]]
    // Keep keyboard focus on the button that was pressed (its neighbour may now be disabled).
    const atEdge = j === 0 || j === steps.length - 1
    pendingFocus.current = `${uid}-${id}-${dir === -1 ? (atEdge ? 'down' : 'up') : atEdge ? 'up' : 'down'}`
    setSteps(steps)
  }

  const editParam = (id: string, patch: { key?: string; value?: string }) => onChange(withSpec(state, state.steps, state.params.map((p) => (p.id === id ? { ...p, ...patch } : p))))

  const toggleAdvanced = () => {
    if (state.advanced) {
      const parsed = jsonToSpec(state.json)
      if (!('error' in parsed)) onChange(withSpec({ ...state, advanced: false }, parsed.steps, parsed.params))
    } else {
      onChange(withSpec({ ...state, advanced: true }, state.steps, state.params))
    }
  }

  return (
    <div className="stack">
      <div className="row">
        <p className="eyebrow">Steps</p>
        <button type="button" className="btn-secondary" aria-pressed={state.advanced} onClick={toggleAdvanced} disabled={!canLeaveJson}>
          {state.advanced ? 'Back to the step editor' : 'Advanced: edit as JSON'}
        </button>
      </div>

      {state.advanced ? (
        <label className="field">
          Workflow spec (JSON)
          <textarea
            className="codeedit"
            rows={14}
            value={state.json}
            onChange={(e) => onChange(withSpecJson(state, e.target.value))}
            aria-invalid={!!jsonProblem}
            aria-describedby={`${uid}-json-msg`}
            spellCheck={false}
          />
          <span id={`${uid}-json-msg`} className={jsonProblem ? 'warning' : 'faint'} role={jsonProblem ? 'alert' : undefined}>
            {jsonProblem ?? 'An object with "steps" and optional "parameters". Each step has a name, type, optional dependsOn and its job, script or webhook payload.'}
          </span>
          {!canLeaveJson && <span className="faint">The step editor cannot show this JSON yet; fix or keep editing it here.</span>}
        </label>
      ) : (
        <>
          {state.steps.length === 0 && <p className="faint">No steps yet. A workflow needs at least one.</p>}
          {state.steps.map((step, i) => (
            <StepFields
              key={step.id}
              uid={`${uid}-${step.id}`}
              index={i}
              count={state.steps.length}
              step={step}
              others={state.steps.filter((s) => s.id !== step.id && s.name !== '').map((s) => s.name)}
              errors={showErrors || touched.has(step.id) ? validation.steps[i] : {}}
              onEdit={(patch) => edit(step.id, patch)}
              onRemove={() => remove(step.id)}
              onMove={(dir) => move(step.id, dir)}
            />
          ))}
          <div className="toolbar">
            <button type="button" className="btn-secondary" onClick={add} disabled={state.steps.length >= 100}>
              Add step
            </button>
          </div>
          {validation.list.length > 0 && (showErrors || state.steps.some((s) => touched.has(s.id))) && (
            <ul className="warning" role="alert">
              {validation.list.map((m) => (
                <li key={m}>{m}</li>
              ))}
            </ul>
          )}
          <DagPreview steps={state.steps} />

          <fieldset className="stack">
            <legend className="eyebrow">Parameters (optional)</legend>
            <p className="faint">Key/value strings the workflow can read.</p>
            {state.params.map((p, i) => {
              const err = validation.params[i]
              const shown = showErrors || p.key !== '' || p.value !== ''
              return (
                <div key={p.id} className="formgrid">
                  <label className="field">
                    Key
                    <input type="text" value={p.key} onChange={(e) => editParam(p.id, { key: e.target.value })} autoComplete="off" aria-invalid={shown && !!err} aria-describedby={shown && err ? `${uid}-${p.id}-msg` : undefined} />
                    {shown && err && (
                      <span id={`${uid}-${p.id}-msg`} className="warning">
                        {err}
                      </span>
                    )}
                  </label>
                  <label className="field">
                    Value
                    <input type="text" value={p.value} onChange={(e) => editParam(p.id, { value: e.target.value })} autoComplete="off" />
                  </label>
                  <div className="field">
                    <button type="button" className="danger" aria-label={`Remove parameter ${p.key || i + 1}`} onClick={() => onChange(withSpec(state, state.steps, state.params.filter((x) => x.id !== p.id)))}>
                      Remove
                    </button>
                  </div>
                </div>
              )
            })}
            <div className="toolbar">
              <button type="button" className="btn-secondary" onClick={() => onChange(withSpec(state, state.steps, [...state.params, { id: newParamId(), key: '', value: '' }]))} disabled={state.params.length >= 64}>
                Add parameter
              </button>
            </div>
          </fieldset>
        </>
      )}
    </div>
  )
}

function uniqueName(steps: StepDraft[], base: string): string {
  const names = new Set(steps.map((s) => s.name))
  for (let n = steps.length + 1; ; n++) if (!names.has(`${base}-${n}`)) return `${base}-${n}`
}

function StepFields({
  uid,
  index,
  count,
  step,
  others,
  errors,
  onEdit,
  onRemove,
  onMove,
}: {
  uid: string
  index: number
  count: number
  step: StepDraft
  others: string[]
  errors: StepErrors
  onEdit: (patch: Partial<StepDraft>) => void
  onRemove: () => void
  onMove: (dir: -1 | 1) => void
}) {
  const label = step.name || `step ${index + 1}`
  const err = (key: keyof StepErrors) =>
    errors[key] ? (
      <span id={`${uid}-${key}-msg`} className="warning">
        {errors[key]}
      </span>
    ) : null
  const desc = (key: keyof StepErrors) => (errors[key] ? `${uid}-${key}-msg` : undefined)
  // Dependencies that no longer exist stay visible (and flagged) so they can be unticked.
  const options = [...others, ...step.dependsOn.filter((d) => !others.includes(d))]
  const toggleDep = (dep: string, on: boolean) => onEdit({ dependsOn: on ? [...step.dependsOn, dep] : step.dependsOn.filter((d) => d !== dep) })

  return (
    <fieldset className="card">
      <legend className="eyebrow">
        Step {index + 1}: {step.name || 'unnamed'}
      </legend>
      <div className="formgrid">
        <label className="field">
          Name
          <input id={`${uid}-name`} type="text" value={step.name} onChange={(e) => onEdit({ name: e.target.value })} placeholder="train" autoComplete="off" spellCheck={false} aria-invalid={!!errors.name} aria-describedby={desc('name')} />
          {err('name')}
        </label>
        <label className="field">
          Type
          <select value={step.type} onChange={(e) => onEdit({ type: e.target.value as StepType })}>
            <option value="job">job</option>
            <option value="script">script</option>
            <option value="webhook">webhook</option>
          </select>
        </label>
        <label className="field">
          Retries
          <input type="number" min={0} max={10} value={step.retries} onChange={(e) => onEdit({ retries: e.target.value })} aria-invalid={!!errors.retries} aria-describedby={desc('retries')} />
          {err('retries')}
        </label>
        <label className="field">
          Timeout (seconds)
          <input type="number" min={0} max={604800} value={step.timeoutSeconds} onChange={(e) => onEdit({ timeoutSeconds: e.target.value })} aria-invalid={!!errors.timeoutSeconds} aria-describedby={desc('timeoutSeconds')} />
          {errors.timeoutSeconds ? err('timeoutSeconds') : <span className="faint">0 means no limit.</span>}
        </label>
      </div>

      {(step.type === 'job' || step.type === 'script') && (
        <div className="formgrid">
          <label className="field">
            Image
            <input type="text" value={step.image} onChange={(e) => onEdit({ image: e.target.value })} placeholder="registry.example.com/train:latest" autoComplete="off" aria-invalid={!!errors.image} aria-describedby={desc('image')} />
            {err('image')}
          </label>
          {step.type === 'job' && (
            <>
              <label className="field">
                GPUs
                <input type="number" min={0} max={1024} value={step.gpus} onChange={(e) => onEdit({ gpus: e.target.value })} aria-invalid={!!errors.gpus} aria-describedby={desc('gpus')} />
                {err('gpus')}
              </label>
              <label className="field">
                GPU type (optional)
                <input type="text" value={step.gpuType} onChange={(e) => onEdit({ gpuType: e.target.value })} placeholder="A100-80G" autoComplete="off" aria-invalid={!!errors.gpuType} aria-describedby={desc('gpuType')} />
                {err('gpuType')}
              </label>
            </>
          )}
          <label className="field span-all">
            Command{step.type === 'job' ? ' (optional)' : ''}
            <input type="text" className="mono" value={step.command} onChange={(e) => onEdit({ command: e.target.value })} placeholder={"python train.py --epochs 3"} autoComplete="off" spellCheck={false} aria-invalid={!!errors.command} aria-describedby={errors.command ? `${uid}-command-msg` : `${uid}-command-hint`} />
            {errors.command ? err('command') : <span id={`${uid}-command-hint`} className="faint">Split into arguments like a shell; use quotes for values with spaces.</span>}
          </label>
        </div>
      )}

      {step.type === 'webhook' && (
        <div className="formgrid">
          <label className="field">
            URL
            <input type="text" value={step.url} onChange={(e) => onEdit({ url: e.target.value })} placeholder="https://example.com/hook" autoComplete="off" aria-invalid={!!errors.url} aria-describedby={desc('url')} />
            {err('url')}
          </label>
          <label className="field">
            Method
            <select value={step.method} onChange={(e) => onEdit({ method: e.target.value as HttpMethod })}>
              {HTTP_METHODS.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          </label>
          <label className="field span-all">
            Body (optional)
            <textarea className="codeedit compact" value={step.body} onChange={(e) => onEdit({ body: e.target.value })} spellCheck={false} aria-invalid={!!errors.body} aria-describedby={desc('body')} />
            {err('body')}
          </label>
        </div>
      )}

      <fieldset className="stack" aria-describedby={desc('graph')}>
        <legend className="faint">Depends on</legend>
        {options.length === 0 ? (
          <span className="faint">No other steps to depend on.</span>
        ) : (
          <div className="row">
            {options.map((dep) => (
              <label key={dep} className="row">
                <input type="checkbox" checked={step.dependsOn.includes(dep)} onChange={(e) => toggleDep(dep, e.target.checked)} />
                <span className={others.includes(dep) ? undefined : 'warning'}>{dep}</span>
              </label>
            ))}
          </div>
        )}
        {err('graph')}
      </fieldset>

      <div className="toolbar">
        <button id={`${uid}-up`} type="button" className="btn-secondary" disabled={index === 0} onClick={() => onMove(-1)} aria-label={`Move up: step ${label}`}>
          Move up
        </button>
        <button id={`${uid}-down`} type="button" className="btn-secondary" disabled={index === count - 1} onClick={() => onMove(1)} aria-label={`Move down: step ${label}`}>
          Move down
        </button>
        <button type="button" className="danger" onClick={onRemove} aria-label={`Remove step ${label}`}>
          Remove
        </button>
      </div>
    </fieldset>
  )
}

/** Steps grouped by dependency depth, left to right. Updates as the form changes. */
function DagPreview({ steps }: { steps: StepDraft[] }) {
  const named = steps.filter((s) => s.name !== '')
  if (named.length === 0) return null
  const layers = dagLayers(named)
  return (
    <div className="stack">
      <p className="eyebrow">Execution order</p>
      <ol className="sr-only" aria-label="Execution order">
        {describeSteps(named, (s) => `type ${s.type}`).map((line) => (
          <li key={line}>{line}</li>
        ))}
      </ol>
      <div className="row" role="img" aria-label="Diagram of the steps grouped by dependency, left to right. The execution order list describes it.">
        {layers.map((layer, i) => (
          <div key={i} className="row">
            {i > 0 && (
              <span className="faint" aria-hidden="true">
                →
              </span>
            )}
            <div className="stack">
              {layer.map((s) => (
                <span key={s.id} className="pill">
                  <span className="mono">{s.name}</span> <span className="faint">{s.type}</span>
                </span>
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}
