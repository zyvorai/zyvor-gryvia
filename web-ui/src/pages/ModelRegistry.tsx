// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useIsAdmin } from '@/lib/useRole'
import { api } from '@/lib/api'
import type { RegisteredModel } from '@/lib/api'
import { errorMessage } from '@/lib/errors'
import { formatDate, formatRelative } from '@/lib/format'
import { notify } from '@/lib/notify'
import { useNow } from '@/lib/useNow'
import type { FilterDef, SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import ConfirmDialog from '@/components/ConfirmDialog'
import CopyButton from '@/components/CopyButton'
import DataTable, { type Column } from '@/components/DataTable'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

const nameOf = (m: RegisteredModel) => m.metadata?.name || 'unknown'
const keyOf = (m: RegisteredModel) => `${nameOf(m)}-${m.spec?.version ?? ''}`

// A model's stage is its status; there is no separate phase to filter on.
const FILTERS: FilterDef<RegisteredModel>[] = [{ name: 'stage', label: 'Stage', get: (m) => m.spec?.stage }]
const SORTS: Record<string, SortAccessor<RegisteredModel>> = {
  name: nameOf,
  version: (m) => m.spec?.version,
  stage: (m) => m.spec?.stage,
  created: (m) => (m.metadata?.creationTimestamp ? Date.parse(m.metadata.creationTimestamp) : undefined),
}
const searchText = (m: RegisteredModel) => `${nameOf(m)} ${m.spec?.version ?? ''} ${m.spec?.stage ?? ''} ${m.spec?.sourceJob ?? ''}`
const DEFAULT_SORT = { key: 'created', dir: 'desc' as const }

export default function ModelRegistry() {
  useDocumentTitle('Models')
  const [openKey, setOpenKey] = useState<string | null>(null)
  const now = useNow(30000)
  const { data: models, isLoading, isError, error, refetch, isRefetching, dataUpdatedAt } = useQuery({
    queryKey: ['models'],
    queryFn: api.getModels,
    refetchInterval: 30000,
  })

  const table = useTableState({ rows: models, searchText, filters: FILTERS, sortAccessors: SORTS, defaultSort: DEFAULT_SORT })

  if (isError && !models) {
    return (
      <>
        <PageHero eyebrow="Models" title="Registry unavailable." tint="red" />
        <ErrorState title="Could not load the model registry." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  const openModel = models?.find((m) => keyOf(m) === openKey)
  const toggleOpen = (m: RegisteredModel) => setOpenKey(openKey === keyOf(m) ? null : keyOf(m))
  const columns: Column<RegisteredModel>[] = [
    {
      key: 'name',
      header: 'Name',
      sortable: true,
      render: (m) => (
        <button
          type="button"
          className="th-sort"
          aria-expanded={openKey === keyOf(m)}
          aria-controls={openKey === keyOf(m) ? 'model-detail' : undefined}
          onClick={(e) => {
            e.stopPropagation()
            toggleOpen(m)
          }}
        >
          <span className="faint" aria-hidden="true">{openKey === keyOf(m) ? '▾' : '▸'}</span>{' '}
          <b className="mono">{nameOf(m)}</b>
        </button>
      ),
    },
    { key: 'version', header: 'Version', sortable: true, render: (m) => <span className="mono muted">{m.spec?.version ?? '—'}</span> },
    { key: 'stage', header: 'Stage', sortable: true, render: (m) => <StageBadge stage={m.spec?.stage} /> },
    {
      key: 'sourceJob',
      header: 'Source job',
      render: (m) =>
        m.spec?.sourceJob ? (
          <Link to={`/jobs/${encodeURIComponent(m.spec.sourceJob)}`} className="card-link mono" onClick={(e) => e.stopPropagation()}>
            {m.spec.sourceJob}
          </Link>
        ) : (
          <span className="muted">—</span>
        ),
    },
    {
      key: 'created',
      header: 'Created',
      sortable: true,
      render: (m) => (
        <span className="faint" title={formatDate(m.metadata?.creationTimestamp)}>
          {formatRelative(m.metadata?.creationTimestamp, now)}
        </span>
      ),
    },
    { key: 'actions', header: 'Actions', render: (m) => <ModelActions model={m} /> },
  ]

  const count = (stage: string) => models?.filter((m) => m.spec?.stage === stage).length

  return (
    <>
      <PageHero eyebrow="Models" title="Every model, versioned." lede="Versions registered from finished training jobs, promoted from dev to staging to production." />

      <div className="grid">
        <PagePulse
          updatedAt={dataUpdatedAt}
          error={isError ? errorMessage(error) : undefined}
          headline={models ? `${count('production')} of ${models.length} models in production.` : undefined}
          figures={[
            { label: 'Total models', value: models?.length },
            { label: 'Production', value: count('production') },
            { label: 'Staging', value: count('staging') },
            { label: 'Dev', value: count('dev') },
          ]}
        />

        {isError && (
          <div className="span3">
            <ErrorState title="Showing the last data received; refreshing failed." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}

        <section className="card span3">
          <p className="eyebrow">Registry</p>
          <h2 className="card-title">All models</h2>
          {isLoading ? (
            <Skeleton rows={4} />
          ) : (
            <>
              {openModel && (
                <div className="card" id="model-detail" role="region" aria-label={`${nameOf(openModel)} details`}>
                  <div className="row">
                    <p className="eyebrow">Model: {nameOf(openModel)}</p>
                    <button type="button" className="btn-secondary" onClick={() => setOpenKey(null)}>
                      Close details
                    </button>
                  </div>
                  <ModelDetail model={openModel} />
                </div>
              )}
              <DataTable
                caption="Registered models"
                columns={columns}
                state={table}
                rowKey={keyOf}
                onRowClick={toggleOpen}
                actions={
                  <button type="button" className="btn-refresh" onClick={() => refetch()} disabled={isRefetching} aria-busy={isRefetching}>
{isRefetching ? 'Refreshing…' : 'Refresh'}
</button>
                }
                empty={
                  <EmptyState
                    title="No models registered yet."
                    action={
                      <Link to="/jobs/new" className="buttonlike primary">
                        Submit a training job
                      </Link>
                    }
                  >
                    Models are registered from training jobs. When a job finishes, its output appears here.
                  </EmptyState>
                }
              />
            </>
          )}
        </section>
      </div>
    </>
  )
}

// --- Sub-components ---

const stageTone: Record<string, string> = {
  dev: '',
  staging: 'info',
  production: 'ok',
  archived: '',
}

const PROMOTE_TARGETS: Record<string, string> = {
  dev: 'staging',
  staging: 'production',
  production: 'archived',
}

function StageBadge({ stage }: { stage?: string }) {
  if (!stage) return <span className="faint">—</span>
  return <span className={`pill ${stageTone[stage] ?? ''}`}>{stage}</span>
}

function ModelDetail({ model }: { model: RegisteredModel }) {
  const stage = model.spec?.stage
  const sourceJob = model.spec?.sourceJob
  const endpoint = model.status?.servingEndpoint
  return (
    <div className="formgrid">
      <div>
        <div className="faint">Artifact paths</div>
        {model.spec?.artifacts && model.spec.artifacts.length > 0 ? (
          <div className="stack">
            {model.spec.artifacts.map((a) => (
              <div key={a} className="toolbar">
                <span className="mono muted">{a}</span>
                <CopyButton value={a} label="artifact path" />
              </div>
            ))}
          </div>
        ) : (
          <div className="faint">No artifacts recorded for this version.</div>
        )}
      </div>
      <div>
        <div className="faint">Source job</div>
        {sourceJob ? (
          <Link to={`/jobs/${encodeURIComponent(sourceJob)}`} className="card-link mono">
            {sourceJob}
          </Link>
        ) : (
          <div className="faint">No source job</div>
        )}
      </div>
      <div>
        <div className="faint">Serving endpoint</div>
        {stage === 'production' && endpoint ? (
          <div className="toolbar">
            <span className="mono muted">{endpoint}</span>
            <CopyButton value={endpoint} label="endpoint" />
          </div>
        ) : (
          <div className="faint">{stage === 'production' ? 'Not deployed' : 'Available in production'}</div>
        )}
      </div>
    </div>
  )
}

function useSettleMutation(action: 'approve' | 'reject', name: string, queryClient: ReturnType<typeof useQueryClient>) {
  return useMutation({
    mutationFn: () => (action === 'approve' ? api.approveModelPromotion(name) : api.rejectModelPromotion(name)),
    onSuccess: () => {
      notify.success(action === 'approve' ? `Approved: ${name} promoted` : `Rejected the promotion of ${name}`)
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
    onError: (err) => notify.error(`Could not ${action} ${name}`, err),
  })
}

/** Row actions: promote to the next stage (with confirmation) and serve the model. */
function ModelActions({ model }: { model: RegisteredModel }) {
  const queryClient = useQueryClient()
  const [confirming, setConfirming] = useState(false)

  const name = nameOf(model)
  const stage = model.spec?.stage
  const version = model.spec?.version
  const target = stage ? PROMOTE_TARGETS[stage] : undefined
  const dangerous = target === 'production' || target === 'archived'

  const promoteMutation = useMutation({
    mutationFn: (targetStage: string) => api.promoteModel(name, targetStage),
    onSuccess: (data, targetStage) => {
      // 202: the gateway asks for an administrator's approval instead of promoting.
      if (data.pendingPromotion) notify.success(`Requested promotion of ${name} to ${targetStage}; an administrator must approve it`)
      else notify.success(`Promoted ${name} to ${targetStage}`)
      queryClient.invalidateQueries({ queryKey: ['models'] })
      setConfirming(false)
    },
    // The dialog stays open and shows the gateway's reason (e.g. a 409 conflict) inline too.
    onError: (err) => notify.error(`Could not promote ${name}`, err),
  })

  const pending = model.pendingPromotion
  const isAdmin = useIsAdmin()
  const approve = useSettleMutation('approve', name, queryClient)
  const reject = useSettleMutation('reject', name, queryClient)

  return (
    // The row toggles its details on click; these controls must not.
    <div className="toolbar" onClick={(e) => e.stopPropagation()}>
      {pending && (
        <>
          <span className="pill warn" title={`Requested by ${pending.by} at ${pending.at}`}>
            Awaiting approval for {pending.target}
          </span>
          {isAdmin && (
            <>
              <button type="button" className="btn-secondary" disabled={approve.isPending} onClick={() => approve.mutate()} aria-label={`Approve promotion: ${name}`}>
                Approve
              </button>
              <button type="button" className="btn-secondary" disabled={reject.isPending} onClick={() => reject.mutate()} aria-label={`Reject promotion: ${name}`}>
                Reject
              </button>
            </>
          )}
        </>
      )}
      {stage !== 'archived' && (
        <Link to={`/inference?new=1&model=${encodeURIComponent(name)}`} className="buttonlike btn-secondary" aria-label={`Serve this model: ${name}`}>
          Serve this model
        </Link>
      )}
      {target && !pending && (
        <button
          type="button"
          className="btn-secondary"
          onClick={() => {
            promoteMutation.reset()
            setConfirming(true)
          }}
          disabled={promoteMutation.isPending}
          aria-label={`Promote to ${target}: ${name}`}
        >
          {promoteMutation.isPending ? 'Promoting…' : `Promote to ${target}`}
        </button>
      )}

      {confirming && target && (
        <ConfirmDialog
          title={`Promote ${name} to ${target}?`}
          confirmLabel={`Promote to ${target}`}
          tone={dangerous ? 'danger' : 'primary'}
          busy={promoteMutation.isPending}
          onCancel={() => setConfirming(false)}
          onConfirm={() => promoteMutation.mutate(target)}
        >
          <p>
            {name}
            {version ? ` (${version})` : ''} moves from <b>{stage}</b> to <b>{target}</b>.
          </p>
          {target === 'production' && <p>Production is the version consumers are expected to serve.</p>}
          {target === 'archived' && <p>Archived models are retired and cannot be promoted again from here.</p>}
          {promoteMutation.isError && (
            <p className="warning" role="alert">
              {errorMessage(promoteMutation.error)}
            </p>
          )}
        </ConfirmDialog>
      )}
    </div>
  )
}
