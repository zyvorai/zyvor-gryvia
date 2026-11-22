// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useId, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { notify } from '@/lib/notify'
import {
  DEFAULT_SETTINGS,
  buildBody,
  curlSnippet,
  keyError,
  pythonSnippet,
  usageLabel,
  type PlaygroundMessage,
  type PlaygroundSettings,
  type TokenUsage,
} from '@/lib/playground'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import PageHero from '@/components/PageHero'
import CopyButton from '@/components/CopyButton'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

interface Turn {
  message: PlaygroundMessage
  usage?: TokenUsage
  error?: string
}

export default function Playground() {
  useDocumentTitle('Playground')
  const ids = useId()
  const models = useQuery({ queryKey: ['llm-models'], queryFn: api.getLlmModels, refetchInterval: 30000 })
  // The key lives in this component's state only: never in storage, never sent anywhere but /api/llm/chat.
  const [key, setKey] = useState('')
  const [settings, setSettings] = useState<PlaygroundSettings>(DEFAULT_SETTINGS)
  const [turns, setTurns] = useState<Turn[]>([])
  const [draft, setDraft] = useState('')
  const [pending, setPending] = useState(false)
  const [snippet, setSnippet] = useState<'curl' | 'python'>('curl')
  const abort = useRef<AbortController | null>(null)

  const hero = (
    <PageHero
      eyebrow="Playground"
      title="Try a model, then ship the call."
      lede="Chat with any model published on the LLM gateway using one of your keys. Requests go through the gateway, so they count against your token quota and appear in usage. Copy the settings as curl or Python when they work."
    />
  )
  if (models.isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={4} />
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

  const enabled = models.data?.enabled ?? false
  const ready = (models.data?.items ?? []).filter((m) => m.ready)
  const names = Array.from(new Set(ready.map((m) => m.model)))
  const model = settings.model && names.includes(settings.model) ? settings.model : names[0] ?? ''
  const current = { ...settings, model }
  const set = <K extends keyof PlaygroundSettings>(k: K, v: PlaygroundSettings[K]) => setSettings((s) => ({ ...s, [k]: v }))
  const keyProblem = keyError(key)
  const history = turns.filter((t) => !t.error).map((t) => t.message)
  const preview = buildBody(current, [...history, ...(draft.trim() ? [{ role: 'user' as const, content: draft.trim() }] : [])])
  const gatewayURL = models.data?.gatewayURL ?? ''

  const send = async () => {
    const content = draft.trim()
    if (!content || pending || keyProblem || !model) return
    const asked: PlaygroundMessage[] = [...history, { role: 'user', content }]
    const body = buildBody(current, asked)
    setTurns((t) => [...t, { message: { role: 'user', content } }, { message: { role: 'assistant', content: '' } }])
    setDraft('')
    setPending(true)
    const update = (f: (last: Turn) => Turn) => setTurns((t) => [...t.slice(0, -1), f(t[t.length - 1])])
    try {
      if (current.stream) {
        abort.current = new AbortController()
        await api.streamLlmChat(
          body,
          key,
          (c) =>
            update((last) => ({
              ...last,
              message: { ...last.message, content: last.message.content + (c.delta ?? '') },
              usage: c.usage ?? last.usage,
              error: c.error ?? last.error,
            })),
          abort.current.signal,
        )
      } else {
        const reply = await api.llmChat(body, key)
        update((last) => ({ ...last, message: { ...last.message, content: reply.text }, usage: reply.usage }))
      }
    } catch (err) {
      if ((err as Error)?.name !== 'AbortError') {
        notify.error(`${model} did not answer`, err)
        update((last) => ({ ...last, error: err instanceof Error ? err.message : String(err) }))
      }
    } finally {
      abort.current = null
      setPending(false)
    }
  }

  const code = snippet === 'curl' ? curlSnippet(gatewayURL, preview) : pythonSnippet(gatewayURL, preview)

  return (
    <>
      {hero}
      <div className="grid">
        {!enabled && (
          <div className="span3 warning" role="status">
            The LLM gateway is not enabled. Install the chart with <span className="mono">--set llmGateway.enabled=true</span>.
          </div>
        )}
        <section className="card">
          <p className="eyebrow">Settings</p>
          <h2 className="card-title">Model and parameters</h2>
          {names.length === 0 ? (
            <EmptyState title="No ready models.">Publish a GryviaInferenceService with gryvia.io/llm-model on the LLM gateway page.</EmptyState>
          ) : (
            <div style={{ display: 'grid', gap: '0.75rem' }}>
              <label htmlFor={`${ids}-model`}>Model</label>
              <select id={`${ids}-model`} value={model} onChange={(e) => set('model', e.target.value)}>
                {names.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
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
                aria-describedby={`${ids}-key-hint`}
              />
              <small id={`${ids}-key-hint`} className="muted">
                {key && keyProblem ? keyProblem : 'Kept in this page only; reloading forgets it.'}
              </small>
              <label htmlFor={`${ids}-system`}>System prompt</label>
              <textarea id={`${ids}-system`} rows={3} className="codeedit compact" value={settings.systemPrompt} onChange={(e) => set('systemPrompt', e.target.value)} />
              <label htmlFor={`${ids}-temp`}>Temperature {settings.temperature.toFixed(1)}</label>
              <input id={`${ids}-temp`} type="range" min={0} max={2} step={0.1} value={settings.temperature} onChange={(e) => set('temperature', Number(e.target.value))} />
              <label htmlFor={`${ids}-max`}>Max tokens</label>
              <input
                id={`${ids}-max`}
                type="number"
                min={1}
                max={32768}
                value={settings.maxTokens}
                onChange={(e) => set('maxTokens', Math.min(32768, Math.max(1, Math.round(Number(e.target.value) || 1))))}
              />
              <label>
                <input type="checkbox" checked={settings.stream} onChange={(e) => set('stream', e.target.checked)} /> Stream the reply
              </label>
            </div>
          )}
        </section>
        <section className="card span2">
          <p className="eyebrow" id={`${ids}-chat`}>
            Chat
          </p>
          <h2 className="card-title">{model || 'No model'}</h2>
          {turns.length > 0 && (
            <ol aria-live="polite" style={{ listStyle: 'none', padding: 0, display: 'grid', gap: '0.75rem' }}>
              {turns.map((t, i) => (
                <li key={i}>
                  <strong>{t.message.role === 'user' ? 'You' : model}</strong>
                  <p style={{ whiteSpace: 'pre-wrap', margin: '0.25rem 0' }}>{t.message.content || (pending && i === turns.length - 1 ? '…' : '')}</p>
                  {t.error && <small className="text-bad" role="alert">{t.error}</small>}
                  {t.message.role === 'assistant' && !t.error && !(pending && i === turns.length - 1) && (
                    <small className="muted" data-testid="usage">
                      {usageLabel(t.usage)}
                    </small>
                  )}
                </li>
              ))}
            </ol>
          )}
          <textarea
            aria-labelledby={`${ids}-chat`}
            rows={3}
            className="codeedit compact"
            placeholder={keyProblem ? 'Paste an LLM key first' : 'Ask the model… (Ctrl+Enter sends)'}
            disabled={!model}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) send()
            }}
          />
          <div style={{ display: 'flex', gap: '0.5rem', marginTop: '0.5rem' }}>
            <button type="button" className="primary" disabled={!model || Boolean(keyProblem) || !draft.trim() || pending} onClick={send}>
              {pending ? 'Answering…' : 'Send'}
            </button>
            {pending && current.stream && (
              <button type="button" className="btn-secondary" onClick={() => abort.current?.abort()}>
                Stop
              </button>
            )}
            <button type="button" className="btn-secondary" disabled={turns.length === 0 || pending} onClick={() => setTurns([])}>
              Clear
            </button>
          </div>
        </section>
        {model && (
          <section className="card span3">
            <p className="eyebrow">Code</p>
            <h2 className="card-title">Call the gateway with these settings</h2>
            <div role="tablist" aria-label="Snippet language" style={{ display: 'flex', gap: '0.5rem', marginBottom: '0.5rem' }}>
              {(['curl', 'python'] as const).map((l) => (
                <button key={l} type="button" role="tab" aria-selected={snippet === l} className={snippet === l ? 'primary' : 'btn-secondary'} onClick={() => setSnippet(l)}>
                  {l === 'curl' ? 'curl' : 'Python'}
                </button>
              ))}
              <CopyButton value={code} label={`${snippet === 'curl' ? 'curl' : 'Python'} snippet`} />
            </div>
            <pre className="mono" style={{ whiteSpace: 'pre-wrap' }} data-testid="snippet">
              {code}
            </pre>
          </section>
        )}
      </div>
    </>
  )
}
