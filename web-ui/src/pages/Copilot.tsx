import { useId, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { notify } from '@/lib/notify'
import { keyError } from '@/lib/playground'
import { EXAMPLES, historyFor, toolsLabel, type CopilotTurn } from '@/lib/copilot'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import PageHero from '@/components/PageHero'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

export default function Copilot() {
  useDocumentTitle('Copilot')
  const ids = useId()
  const models = useQuery({ queryKey: ['llm-models'], queryFn: api.getLlmModels, refetchInterval: 30000 })
  // As in the playground, the key lives in this component's state only.
  const [key, setKey] = useState('')
  const [chosen, setChosen] = useState('')
  const [turns, setTurns] = useState<CopilotTurn[]>([])
  const [draft, setDraft] = useState('')
  const [pending, setPending] = useState(false)

  const hero = (
    <PageHero
      eyebrow="Copilot"
      title="Ask about your platform."
      lede="Ask about your jobs, models, datasets and services. It reads what you can open yourself. When approved operations are enabled, named administrators can request proposals for separate human review in Intelligence. It never approves or executes them."
    />
  )
  if (models.isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={3} />
      </>
    )
  }
  if (models.isError && !models.data) {
    return (
      <>
        {hero}
        <ErrorState title="Could not load models." error={models.error} onRetry={() => models.refetch()} retrying={models.isRefetching} />
      </>
    )
  }

  const names = Array.from(new Set((models.data?.items ?? []).filter((m) => m.ready).map((m) => m.model)))
  const model = chosen && names.includes(chosen) ? chosen : names[0] ?? ''
  const keyProblem = keyError(key)

  const ask = async (text: string) => {
    const question = text.trim()
    if (!question || pending || keyProblem || !model) return
    const history = historyFor(turns)
    setTurns((t) => [...t, { role: 'user', content: question }, { role: 'assistant', content: '' }])
    setDraft('')
    setPending(true)
    const finish = (f: (last: CopilotTurn) => CopilotTurn) => setTurns((t) => [...t.slice(0, -1), f(t[t.length - 1])])
    try {
      const reply = await api.copilotChat({ model, question, history }, key)
      finish((last) => ({ ...last, content: reply.answer, tools: reply.tools }))
    } catch (err) {
      notify.error('The copilot did not answer', err)
      finish((last) => ({ ...last, error: err instanceof Error ? err.message : String(err) }))
    } finally {
      setPending(false)
    }
  }

  return (
    <>
      {hero}
      {(models.data?.enabled ?? false) === false && (
        <div className="warning" role="status">
          The LLM gateway is not enabled. Install the chart with <span className="mono">--set llmGateway.enabled=true</span>.
        </div>
      )}
      <div className="grid">
        <section className="card">
          <p className="eyebrow">Settings</p>
          <h2 className="card-title">Model and key</h2>
          {names.length === 0 ? (
            <EmptyState title="No ready models.">Publish a model on the LLM gateway page first.</EmptyState>
          ) : (
            <div style={{ display: 'grid', gap: '0.75rem' }}>
              <label htmlFor={`${ids}-model`}>Model</label>
              <select id={`${ids}-model`} value={model} onChange={(e) => setChosen(e.target.value)}>
                {names.map((n) => (
                  <option key={n}>{n}</option>
                ))}
              </select>
              <label htmlFor={`${ids}-key`}>LLM key</label>
              <input
                id={`${ids}-key`}
                type="password"
                autoComplete="off"
                spellCheck={false}
                placeholder="gk-…"
                value={key}
                onChange={(e) => setKey(e.target.value.trim())}
                aria-invalid={key ? Boolean(keyProblem) : undefined}
              />
              <small className="muted">{key && keyProblem ? keyProblem : 'Kept in this page only; reloading forgets it.'}</small>
              <small className="muted">A model that supports tool calling works best.</small>
            </div>
          )}
        </section>
        <section className="card span2">
          <p className="eyebrow" id={`${ids}-chat`}>
            Conversation
          </p>
          {turns.length === 0 && (
            <ul className="stack">
              {EXAMPLES.map((q) => (
                <li key={q}>
                  <button type="button" className="btn-secondary" disabled={!model || Boolean(keyProblem) || pending} onClick={() => ask(q)}>
                    {q}
                  </button>
                </li>
              ))}
            </ul>
          )}
          <ol aria-live="polite" style={{ listStyle: 'none', padding: 0, display: 'grid', gap: '0.75rem' }}>
            {turns.map((t, i) => (
              <li key={i}>
                <strong>{t.role === 'user' ? 'You' : 'Copilot'}</strong>
                <p style={{ whiteSpace: 'pre-wrap', margin: '0.25rem 0' }}>{t.content || (pending && i === turns.length - 1 ? '…' : '')}</p>
                {t.error && (
                  <small className="text-bad" role="alert">
                    {t.error}
                  </small>
                )}
                {toolsLabel(t.tools) && <small className="muted">{toolsLabel(t.tools)}</small>}
              </li>
            ))}
          </ol>
          <textarea
            aria-labelledby={`${ids}-chat`}
            rows={3}
            className="codeedit compact"
            placeholder={keyProblem ? 'Paste an LLM key first' : 'Ask a question… (Ctrl+Enter sends)'}
            disabled={!model}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) ask(draft)
            }}
          />
          <div style={{ display: 'flex', gap: '0.5rem', marginTop: '0.5rem' }}>
            <button type="button" className="primary" disabled={!model || Boolean(keyProblem) || !draft.trim() || pending} onClick={() => ask(draft)}>
              {pending ? 'Thinking…' : 'Ask'}
            </button>
            <button type="button" className="btn-secondary" disabled={turns.length === 0 || pending} onClick={() => setTurns([])}>
              Clear
            </button>
          </div>
        </section>
      </div>
    </>
  )
}
