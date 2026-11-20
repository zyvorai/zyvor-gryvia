// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useId, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { errorMessage } from '@/lib/errors'
import { notify } from '@/lib/notify'
import { fieldAria } from '@/lib/forms'
import { formatMoney, formatNumber, formatRelative } from '@/lib/format'
import { curlExample, keyNameError, priceLabel, quotaPercent, quotaTone, type CreatedLlmKey, type LlmKey, type LlmModel, type LlmUsage } from '@/lib/llm'
import type { SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import DataTable, { type Column } from '@/components/DataTable'
import PageHero from '@/components/PageHero'
import Modal from '@/components/Modal'
import ConfirmDialog from '@/components/ConfirmDialog'
import CopyButton from '@/components/CopyButton'
import Progress from '@/components/Progress'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

const MODEL_SORTS: Record<string, SortAccessor<LlmModel>> = { model: (m) => m.model, namespace: (m) => m.namespace }
const KEY_SORTS: Record<string, SortAccessor<LlmKey>> = { name: (k) => k.name, namespace: (k) => k.namespace }
const GROUPS: LlmUsage['groupBy'][] = ['model', 'tenant', 'namespace', 'day']

export default function LlmGateway() {
  useDocumentTitle('LLM gateway')
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [revoking, setRevoking] = useState<LlmKey | null>(null)
  const [groupBy, setGroupBy] = useState<LlmUsage['groupBy']>('model')

  const models = useQuery({ queryKey: ['llm-models'], queryFn: api.getLlmModels, refetchInterval: 30000 })
  const enabled = models.data?.enabled ?? false
  const keys = useQuery({ queryKey: ['llm-keys'], queryFn: api.getLlmKeys, enabled, refetchInterval: 60000 })
  const usage = useQuery({ queryKey: ['llm-usage', groupBy], queryFn: () => api.getLlmUsage(groupBy), refetchInterval: 60000 })

  const modelTable = useTableState({ rows: models.data?.items, searchText: (m) => `${m.model} ${m.namespace} ${m.service}`, sortAccessors: MODEL_SORTS, defaultSort: { key: 'model', dir: 'asc' }, pageSize: 10 })
  const keyTable = useTableState({ rows: keys.data, searchText: (k) => `${k.name} ${k.namespace} ${k.tenant} ${k.description ?? ''}`, sortAccessors: KEY_SORTS, defaultSort: { key: 'namespace', dir: 'asc' }, pageSize: 10 })

  const revoke = useMutation({
    mutationFn: (k: LlmKey) => api.deleteLlmKey(k.id),
    onSuccess: (_d, k) => {
      notify.success(`Revoked key ${k.name}`)
      setRevoking(null)
      return queryClient.invalidateQueries({ queryKey: ['llm-keys'] })
    },
    onError: (err, k) => {
      notify.error(`Could not revoke ${k.name}`, err)
      setRevoking(null)
    },
  })

  const hero = (
    <PageHero
      eyebrow="LLM gateway"
      title="One OpenAI endpoint for every model."
      lede="Inference services annotated gryvia.io/llm-model are served behind one OpenAI-compatible API. Each tenant calls it with its own keys; tokens are metered and capped by GryviaQuota tokensPerDay."
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
        <PageHero eyebrow="LLM gateway" title="LLM gateway unavailable." tint="red" />
        <ErrorState title="Could not load models." error={models.error} onRetry={() => models.refetch()} retrying={models.isRefetching} />
      </>
    )
  }

  const gatewayURL = models.data?.gatewayURL ?? ''
  const modelColumns: Column<LlmModel>[] = [
    { key: 'model', header: 'Model', sortable: true, render: (m) => <span className="mono">{m.model}</span> },
    { key: 'namespace', header: 'Namespace', sortable: true, render: (m) => m.namespace },
    { key: 'service', header: 'Service', render: (m) => <span className="mono">{m.service}</span> },
    { key: 'shared', header: 'Shared', render: (m) => (m.shared ? 'yes' : 'no') },
    { key: 'status', header: 'Status', render: (m) => <span className={`pill ${m.ready ? 'ok' : 'warn'}`}>{m.ready ? 'ready' : m.phase || 'pending'}</span> },
    { key: 'price', header: 'Price in / out per 1M', render: (m) => priceLabel(m) },
  ]
  const keyColumns: Column<LlmKey>[] = [
    { key: 'name', header: 'Key', sortable: true, render: (k) => <span className="mono">{k.name}</span> },
    { key: 'namespace', header: 'Namespace', sortable: true, render: (k) => k.namespace },
    { key: 'tenant', header: 'Tenant', render: (k) => k.tenant },
    { key: 'prefix', header: 'Starts with', render: (k) => <span className="mono">{k.prefix}…</span> },
    { key: 'description', header: 'Description', render: (k) => k.description || <span className="faint">—</span> },
    { key: 'created', header: 'Created', render: (k) => (k.created ? formatRelative(k.created) : '—') },
    {
      key: 'actions',
      header: 'Actions',
      render: (k) => (
        <button type="button" className="danger" onClick={() => setRevoking(k)} aria-label={`Revoke key ${k.name}`}>
          Revoke
        </button>
      ),
    },
  ]

  return (
    <>
      {hero}
      <div className="grid">
        {!enabled && (
          <div className="span3 warning" role="status">
            The LLM gateway is not enabled. Install the chart with <span className="mono">--set llmGateway.enabled=true</span> to issue keys.
          </div>
        )}
        <section className="card span3">
          <p className="eyebrow">Models</p>
          <h2 className="card-title">Published models</h2>
          {gatewayURL && (
            <p>
              Endpoint <span className="mono">{gatewayURL}/v1</span> <CopyButton value={`${gatewayURL}/v1`} label="endpoint" />
            </p>
          )}
          <DataTable
            caption="Models served by the LLM gateway"
            columns={modelColumns}
            state={modelTable}
            rowKey={(m) => `${m.namespace}/${m.model}`}
            searchLabel="Search models"
            empty={<EmptyState title="No models published.">Annotate a GryviaInferenceService with gryvia.io/llm-model: &lt;name&gt;.</EmptyState>}
          />
        </section>
        {enabled && (
          <section className="card span3">
            <p className="eyebrow">Keys</p>
            <h2 className="card-title">API keys</h2>
            {keys.isError ? (
              <ErrorState title="Could not load keys." error={keys.error} onRetry={() => keys.refetch()} retrying={keys.isRefetching} />
            ) : (
              <DataTable
                caption="LLM gateway API keys"
                columns={keyColumns}
                state={keyTable}
                rowKey={(k) => k.id}
                searchLabel="Search keys"
                actions={
                  <button type="button" className="primary" onClick={() => setCreating(true)}>
                    New key
                  </button>
                }
                empty={<EmptyState title="No keys yet.">Create a key and call the gateway with it as a bearer token.</EmptyState>}
              />
            )}
          </section>
        )}
        <UsageCard usage={usage.data} error={usage.isError ? usage.error : undefined} groupBy={groupBy} setGroupBy={setGroupBy} />
      </div>
      {revoking && (
        <ConfirmDialog title={`Revoke key ${revoking.name}?`} confirmLabel="Revoke key" busy={revoke.isPending} onCancel={() => setRevoking(null)} onConfirm={() => revoke.mutate(revoking)}>
          Requests with this key fail from now on. Usage already recorded is kept.
        </ConfirmDialog>
      )}
      {creating && <KeyModal gatewayURL={gatewayURL} firstModel={models.data?.items[0]?.model ?? 'my-model'} onClose={() => setCreating(false)} />}
    </>
  )
}

function UsageCard({ usage, error, groupBy, setGroupBy }: { usage?: LlmUsage; error?: unknown; groupBy: LlmUsage['groupBy']; setGroupBy: (g: LlmUsage['groupBy']) => void }) {
  return (
    <section className="card span3">
      <p className="eyebrow">Usage</p>
      <h2 className="card-title">Tokens, last 30 days</h2>
      <div className="toolbar" role="group" aria-label="Group usage by">
        {GROUPS.map((g) => (
          <button key={g} type="button" className={g === groupBy ? 'primary' : 'btn-secondary'} aria-pressed={g === groupBy} onClick={() => setGroupBy(g)}>
            {g}
          </button>
        ))}
      </div>
      {error ? (
        <ErrorState title="Could not load usage." error={error} />
      ) : !usage ? (
        <Skeleton rows={2} />
      ) : (
        <>
          {usage.quotas.length > 0 && (
            <div className="stack">
              {usage.quotas.map((q) => (
                <div key={q.name}>
                  <strong>{q.name}</strong>: {formatNumber(q.usedToday)} of {formatNumber(q.tokensPerDay)} tokens today
                  <Progress value={quotaPercent(q)} label={`Daily tokens used by ${q.name}`} tone={quotaTone(q)} />
                </div>
              ))}
            </div>
          )}
          {usage.items.length === 0 ? (
            <EmptyState title="No token usage recorded.">Records are written every 5 minutes, one per tenant, model and hour.</EmptyState>
          ) : (
            <div className="table-wrap">
              <table>
                <caption className="sr-only">Token usage by {usage.groupBy}</caption>
                <thead>
                  <tr>
                    <th scope="col">{usage.groupBy}</th>
                    <th scope="col" className="num">Input</th>
                    <th scope="col" className="num">Output</th>
                    <th scope="col" className="num">Cost</th>
                  </tr>
                </thead>
                <tbody>
                  {usage.items.map((g) => (
                    <tr key={String(g[usage.groupBy])}>
                      <td className="mono">{String(g[usage.groupBy]) || '—'}</td>
                      <td className="num">{formatNumber(g.inputTokens)}</td>
                      <td className="num">{formatNumber(g.outputTokens)}</td>
                      <td className="num">{formatMoney(g.cost)}</td>
                    </tr>
                  ))}
                  <tr>
                    <th scope="row">Total</th>
                    <td className="num">{formatNumber(usage.total.inputTokens)}</td>
                    <td className="num">{formatNumber(usage.total.outputTokens)}</td>
                    <td className="num">{formatMoney(usage.total.cost)}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  )
}

function KeyModal({ gatewayURL, firstModel, onClose }: { gatewayURL: string; firstModel: string; onClose: () => void }) {
  const queryClient = useQueryClient()
  const uid = useId()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [touched, setTouched] = useState(false)
  const [created, setCreated] = useState<CreatedLlmKey | null>(null)
  const nameError = touched ? keyNameError(name) : undefined

  const save = useMutation({
    mutationFn: () => api.createLlmKey({ name, description: description || undefined }),
    onSuccess: (k) => {
      setCreated(k)
      notify.success(`Key ${k.name} created`)
      queryClient.invalidateQueries({ queryKey: ['llm-keys'] })
    },
  })
  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setTouched(true)
    if (keyNameError(name)) return
    save.mutate()
  }

  if (created) {
    return (
      <Modal title={`Key ${created.name}`} onClose={onClose}>
        <div className="stack">
          <p className="warning" role="alert">
            Store this key now. Only its hash is kept, so it cannot be shown again.
          </p>
          <p>
            <span className="mono">{created.key}</span> <CopyButton value={created.key} label="key" />
          </p>
          <p>
            It works for the models of namespace <span className="mono">{created.namespace}</span> and the shared models.
          </p>
          <pre className="mono">{curlExample(created.gatewayURL || gatewayURL, firstModel)}</pre>
          <div className="toolbar">
            <button type="button" className="primary" onClick={onClose}>
              Done
            </button>
          </div>
        </div>
      </Modal>
    )
  }
  return (
    <Modal title="New API key" onClose={onClose} dirty={!!name || !!description}>
      <form onSubmit={submit} className="stack" noValidate>
        <label className="field">
          Name
          <input type="text" data-autofocus value={name} onChange={(e) => setName(e.target.value)} placeholder="ci-runner" autoComplete="off" spellCheck={false} {...fieldAria(`${uid}-name`, nameError)} />
          {nameError && (
            <span id={`${uid}-name-msg`} className="warning">
              {nameError}
            </span>
          )}
        </label>
        <label className="field">
          Description (optional)
          <input type="text" value={description} maxLength={256} onChange={(e) => setDescription(e.target.value)} autoComplete="off" />
        </label>
        {save.isError && (
          <p className="warning" role="alert">
            {errorMessage(save.error)}
          </p>
        )}
        <div className="toolbar">
          <button type="button" className="btn-secondary" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={save.isPending}>
            {save.isPending ? 'Creating…' : 'Create key'}
          </button>
        </div>
      </form>
    </Modal>
  )
}
