import { useId, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import PageHero from '@/components/PageHero'
import { ErrorState, Skeleton } from '@/components/StateViews'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useIsAdmin } from '@/lib/useRole'
import { notify } from '@/lib/notify'
import {
  AREAS,
  initial,
  intelligenceApi,
  label,
  resolved,
  type Schema,
} from '@/lib/intelligence'

function Fields({
  schema,
  root,
  value,
  change,
  title,
}: {
  schema: Schema
  root: Schema
  value: unknown
  change: (v: unknown) => void
  title: string
}) {
  const id = useId()
  const s = resolved(schema, root)
  if (s.type === 'object') {
    const obj = (value ?? {}) as Record<string, unknown>
    return (
      <fieldset
        style={{
          display: 'grid',
          gap: '0.75rem',
          border: '1px solid var(--border)',
          padding: '1rem',
        }}
      >
        <legend>{title}</legend>
        {Object.entries(s.properties ?? {}).map(([key, sub]) => {
          const optional = !(s.required ?? []).includes(key)
          const exists = obj[key] !== undefined && obj[key] !== null
          return (
            <div key={key}>
              {optional && (
                <label>
                  <input
                    type="checkbox"
                    checked={exists}
                    onChange={(e) => {
                      const next = { ...obj }
                      if (e.target.checked) next[key] = initial(sub, root)
                      else delete next[key]
                      change(next)
                    }}
                  />{' '}
                  Include {label(key)}
                </label>
              )}
              {(!optional || exists) && (
                <Fields
                  schema={sub}
                  root={root}
                  value={obj[key]}
                  title={label(key)}
                  change={(v) => change({ ...obj, [key]: v })}
                />
              )}
            </div>
          )
        })}
      </fieldset>
    )
  }
  if (s.type === 'array') {
    const items = (value ?? []) as unknown[]
    const maximum = Math.min(s.maxItems ?? 100, 100)
    return (
      <fieldset style={{ padding: '0.75rem' }}>
        <legend>
          {title} ({items.length})
        </legend>
        {items.map((item, index) => (
          <div key={index} style={{ marginBottom: '0.75rem' }}>
            <Fields
              schema={s.items ?? {}}
              root={root}
              value={item}
              title={`${title} ${index + 1}`}
              change={(v) =>
                change(items.map((old, i) => (i === index ? v : old)))
              }
            />
            <button
              type="button"
              className="btn-secondary"
              aria-label={`Remove ${title} ${index + 1}`}
              onClick={() => change(items.filter((_, i) => i !== index))}
            >
              Remove
            </button>
          </div>
        ))}
        <button
          type="button"
          className="btn-secondary"
          disabled={items.length >= maximum}
          onClick={() => change([...items, initial(s.items ?? {}, root)])}
        >
          Add {title}
        </button>
      </fieldset>
    )
  }
  if (s.type === 'boolean')
    return (
      <label htmlFor={id}>
        <input
          id={id}
          type="checkbox"
          checked={Boolean(value)}
          onChange={(e) => change(e.target.checked)}
        />{' '}
        {title}
      </label>
    )
  return (
    <div style={{ display: 'grid', gap: '0.25rem' }}>
      <label htmlFor={id}>{title}</label>
      {s.enum ? (
        <select
          id={id}
          value={String(value ?? '')}
          onChange={(e) =>
            change(s.enum?.find((v) => String(v) === e.target.value))
          }
        >
          {s.enum.map((v) => (
            <option key={v} value={String(v)}>
              {v}
            </option>
          ))}
        </select>
      ) : (
        <input
          id={id}
          type={s.type === 'integer' || s.type === 'number' ? 'number' : 'text'}
          value={String(value ?? '')}
          required
          min={s.minimum ?? s.exclusiveMinimum}
          max={s.maximum}
          step={s.type === 'integer' ? 1 : 'any'}
          minLength={s.minLength}
          maxLength={s.maxLength}
          placeholder={
            s.format === 'date-time' ? '2026-10-07T10:00:00+05:30' : undefined
          }
          onChange={(e) =>
            change(
              s.type === 'integer' || s.type === 'number'
                ? e.target.value === ''
                  ? ''
                  : Number(e.target.value)
                : e.target.value,
            )
          }
        />
      )}
      {s.description && <small className="muted">{s.description}</small>}
      {s.format === 'date-time' && (
        <small className="muted">
          ISO timestamp with timezone; use the time the observation was
          collected.
        </small>
      )}
    </div>
  )
}

function Result({ value }: { value: unknown }) {
  if (value === null || value === undefined)
    return <span className="muted">Unknown</span>
  if (typeof value === 'boolean') return <span>{value ? 'Yes' : 'No'}</span>
  if (typeof value !== 'object')
    return (
      <span style={{ overflowWrap: 'anywhere' }}>
        {typeof value === 'number'
          ? value.toLocaleString(undefined, { maximumFractionDigits: 6 })
          : String(value)}
      </span>
    )
  if (Array.isArray(value))
    return value.length === 0 ? (
      <span className="muted">None reported</span>
    ) : (
      <ol style={{ paddingLeft: '1.5rem' }}>
        {value.slice(0, 100).map((v, i) => (
          <li key={i} style={{ marginBottom: '0.75rem' }}>
            <Result value={v} />
          </li>
        ))}
        {value.length > 100 && (
          <li>{value.length - 100} more entries in the full report.</li>
        )}
      </ol>
    )
  return (
    <dl style={{ display: 'grid', gap: '0.5rem' }}>
      {Object.entries(value).map(([k, v]) => (
        <div key={k}>
          <dt style={{ fontWeight: 600 }}>{label(k)}</dt>
          <dd style={{ marginLeft: '1rem' }}>
            <Result value={v} />
          </dd>
        </div>
      ))}
    </dl>
  )
}

export default function Intelligence() {
  useDocumentTitle('Workload intelligence')
  const admin = useIsAdmin()
  const schemas = useQuery({
    queryKey: ['intelligence-schemas'],
    queryFn: intelligenceApi.schemas,
  })
  const capabilities = useQuery({
    queryKey: ['intelligence-capabilities'],
    queryFn: intelligenceApi.capabilities,
  })
  const [area, setArea] = useState<string>('preflight')
  const [values, setValues] = useState<Record<string, unknown>>({})
  const [result, setResult] = useState<Record<string, unknown> | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [job, setJob] = useState('')
  const [proposal, setProposal] = useState({
    kind: 'inference-replicas',
    name: '',
    namespace: 'default',
    value: 1,
    reason: '',
    evidence: '',
  })
  const actionsEnabled = admin && Boolean(capabilities.data?.actionsEnabled)
  const actions = useQuery({
    queryKey: ['intelligence-actions'],
    queryFn: intelligenceApi.actions,
    enabled: actionsEnabled,
    retry: false,
  })
  const selected = AREAS.find((a) => a[0] === area) ?? AREAS[0]
  const schema = schemas.data?.[area]
  const input = values[area] ?? (schema ? initial(schema, schema) : {})
  async function run(fn: () => Promise<Record<string, unknown>>) {
    setBusy(true)
    setError(null)
    setResult(null)
    try {
      setResult(await fn())
    } catch (e) {
      setError(e)
    } finally {
      setBusy(false)
    }
  }
  async function operation(fn: () => Promise<unknown>) {
    setBusy(true)
    try {
      await fn()
      await actions.refetch()
      notify.success('Operation recorded')
    } catch (e) {
      notify.error('Operation refused', e)
    } finally {
      setBusy(false)
    }
  }
  function download() {
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(result, null, 2)], { type: 'application/json' }),
    )
    const link = document.createElement('a')
    link.href = url
    link.download = `gryvia-${area}-report.json`
    link.click()
    URL.revokeObjectURL(url)
  }
  return (
    <>
      <PageHero
        eyebrow="Intelligence"
        title="Understand the work. Improve the outcome."
        lede="Preflight, evidence, cost and recovery in one workbench. Supply observations or scenarios; every report states what it can establish."
      />
      <div className="warning" role="status">
        Scenario analyses use the inputs you supply. Cluster observations are
        available below. GPU and fabric qualification still require a real
        workload.
      </div>
      <div className="grid">
        <section className="card">
          <h2 className="card-title">Choose an analysis</h2>
          <label htmlFor="intelligence-area">Analysis</label>
          <select
            id="intelligence-area"
            value={area}
            disabled={busy}
            onChange={(e) => {
              setArea(e.target.value)
              setResult(null)
              setError(null)
            }}
          >
            {AREAS.map(([key, title]) => (
              <option key={key} value={key}>
                {title}
              </option>
            ))}
          </select>
          <p>{selected[2]}</p>
          <h3>Existing job</h3>
          <label htmlFor="intelligence-job">Job name</label>
          <input
            id="intelligence-job"
            value={job}
            onChange={(e) => setJob(e.target.value)}
          />
          <div className="toolbar">
            <button
              type="button"
              disabled={!job || busy}
              onClick={() => run(() => intelligenceApi.explain(job))}
            >
              Explain scheduling
            </button>
            <button
              type="button"
              disabled={!job || busy}
              onClick={() => run(() => intelligenceApi.economics(job))}
            >
              Read metered cost
            </button>
          </div>
          {capabilities.data?.limitations.map((text) => (
            <p key={text} className="muted">
              {text}
            </p>
          ))}
        </section>
        <section className="card span2">
          <h2 className="card-title">{selected[1]}</h2>
          {schemas.isLoading ? (
            <Skeleton rows={4} />
          ) : schemas.isError ? (
            <ErrorState
              title="Could not load analysis fields"
              error={schemas.error}
              onRetry={() => schemas.refetch()}
            />
          ) : (
            schema && (
              <form
                onSubmit={(e) => {
                  e.preventDefault()
                  run(() => intelligenceApi.analyze(area, input))
                }}
              >
                <Fields
                  key={area}
                  schema={schema}
                  root={schema}
                  value={input}
                  title="Inputs"
                  change={(v) => setValues({ ...values, [area]: v })}
                />
                <button
                  className="primary"
                  type="submit"
                  disabled={busy}
                  style={{ marginTop: '1rem' }}
                >
                  {busy ? 'Analyzing…' : 'Analyze'}
                </button>
              </form>
            )
          )}
        </section>
      </div>
      {error != null && (
        <ErrorState title="Analysis could not complete" error={error} />
      )}
      {result && (
        <section className="card" aria-live="polite">
          <h2 className="card-title">Report</h2>
          <button type="button" onClick={download}>
            Download full report
          </button>
          <Result value={result} />
        </section>
      )}
      {actionsEnabled && (
        <section className="card">
          <h2 className="card-title">Approved operations</h2>
          <p>
            Two named OIDC administrators are required. Proposals expire after
            15 minutes. Execution writes desired state; it does not prove
            workload health.
          </p>
          {actions.isError && (
            <ErrorState
              title="Could not read operations"
              error={actions.error}
            />
          )}
          <form
            onSubmit={(e) => {
              e.preventDefault()
              operation(() => intelligenceApi.propose(proposal))
            }}
            style={{ display: 'grid', gap: '0.75rem' }}
          >
            <label>
              Operation
              <select
                value={proposal.kind}
                onChange={(e) =>
                  setProposal({ ...proposal, kind: e.target.value, value: 1 })
                }
              >
                <option value="inference-replicas">
                  Set inference replicas
                </option>
                <option value="quota-max-gpus">Set quota GPU limit</option>
                <option value="node-quarantine">Cordon node</option>
                <option value="aijob-resize">Resize elastic job (workers)</option>
              </select>
            </label>
            <label>
              Resource name
              <input
                required
                value={proposal.name}
                onChange={(e) =>
                  setProposal({ ...proposal, name: e.target.value })
                }
              />
            </label>
            <label>
              Namespace
              <input
                required
                value={proposal.namespace}
                onChange={(e) =>
                  setProposal({ ...proposal, namespace: e.target.value })
                }
              />
            </label>
            <label>
              Desired value
              <input
                type="number"
                required
                min={proposal.kind === 'quota-max-gpus' ? 0 : 1}
                max={
                  proposal.kind === 'node-quarantine'
                    ? 1
                    : proposal.kind === 'inference-replicas'
                      ? 100
                      : proposal.kind === 'aijob-resize'
                        ? 1024
                        : 1048576
                }
                value={proposal.value}
                onChange={(e) =>
                  setProposal({ ...proposal, value: Number(e.target.value) })
                }
              />
            </label>
            <label>
              Reason
              <textarea
                required
                minLength={10}
                maxLength={2000}
                value={proposal.reason}
                onChange={(e) =>
                  setProposal({ ...proposal, reason: e.target.value })
                }
              />
            </label>
            <label>
              Evidence
              <textarea
                required
                minLength={10}
                maxLength={6000}
                value={proposal.evidence}
                onChange={(e) =>
                  setProposal({ ...proposal, evidence: e.target.value })
                }
              />
            </label>
            <button type="submit" disabled={busy}>
              Create proposal
            </button>
          </form>
          {actions.data?.items.map((op) => (
            <article
              key={op.id}
              style={{
                padding: '1rem',
                borderBottom: '1px solid var(--border)',
              }}
            >
              <h3>
                {op.proposal.kind}: {op.proposal.name}
              </h3>
              <p>
                {op.state} · Proposed by {op.proposedBy.subject} · Expires{' '}
                {new Date(op.expiresAt).toLocaleString()}
              </p>
              <p>{op.proposal.reason}</p>
              <p>{op.proposal.evidence}</p>
              <p>
                Previous value: <Result value={op.previousValue} /> → Desired:{' '}
                {op.proposal.value}
              </p>
              <div className="toolbar">
                {(op.state === 'Proposed'
                  ? ['approve', 'reject']
                  : op.state === 'Approved'
                    ? ['execute']
                    : op.state === 'Applied'
                      ? ['rollback']
                      : []
                ).map((t) => (
                  <button
                    type="button"
                    key={t}
                    disabled={busy}
                    onClick={() =>
                      operation(() => intelligenceApi.transition(op.id, t))
                    }
                  >
                    {label(t)}
                  </button>
                ))}
              </div>
              {['Executing', 'RollingBack'].includes(op.state) && (
                <p role="status">
                  Outcome needs manual inspection. This operation cannot be
                  replayed.
                </p>
              )}
            </article>
          ))}
          {actions.data?.truncated && (
            <p>
              Only the first 100 operations are shown. Archive reviewed records
              before creating more.
            </p>
          )}
        </section>
      )}
    </>
  )
}
