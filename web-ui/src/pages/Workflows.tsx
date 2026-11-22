// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '@/lib/api'
import type { Workflow, WorkflowStep } from '@/lib/api'
import { errorMessage } from '@/lib/errors'
import { formatDate, formatRelative } from '@/lib/format'
import { countByGroup, phaseGroup, phaseTone } from '@/lib/phase'
import { notify } from '@/lib/notify'
import { nameError } from '@/lib/forms'
import { dagLayers, describeSteps } from '@/lib/dag'
import { useNow } from '@/lib/useNow'
import { initialSpecState, specBody, specError, type SpecState } from '@/lib/workflowSteps'
import type { FilterDef, SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import ConfirmDialog from '@/components/ConfirmDialog'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import { countTone } from '@/components/kit/tone'
import Modal from '@/components/Modal'
import Progress from '@/components/Progress'
import DataTable, { type Column } from '@/components/DataTable'
import StepEditor from '@/components/StepEditor'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

const phaseOf = (w: Workflow) => w.status?.phase || 'Pending'
const nameOf = (w: Workflow) => w.metadata?.name || 'unknown'

const FILTERS: FilterDef<Workflow>[] = [{ name: 'status', label: 'Status', get: phaseOf }]
const SORTS: Record<string, SortAccessor<Workflow>> = {
  name: nameOf,
  steps: (w) => mergeSteps(w).length,
  status: phaseOf,
  duration: (w) => w.status?.duration,
  started: (w) => (w.status?.startedAt ? Date.parse(w.status.startedAt) : undefined),
}
const searchText = (w: Workflow) => `${nameOf(w)} ${phaseOf(w)}`
const DEFAULT_SORT = { key: 'started', dir: 'desc' as const }

export default function Workflows() {
  useDocumentTitle('Workflows')
  const [showCreate, setShowCreate] = useState(false)
  const [openName, setOpenName] = useState<string | null>(null)
  const queryClient = useQueryClient()
  const [deleting, setDeleting] = useState<string | null>(null)
  const deleteMutation = useMutation({
    mutationFn: (name: string) => api.deleteWorkflow(name),
    onSuccess: (_d, name) => {
      notify.success(`Deleted workflow ${name}`)
      setDeleting(null)
      if (openName === name) setOpenName(null)
      return queryClient.invalidateQueries({ queryKey: ['workflows'] })
    },
    onError: (err, name) => {
      notify.error(`Could not delete ${name}`, err)
      setDeleting(null)
    },
  })
  const now = useNow(30000)
  const { data: workflows, isLoading, isError, error, refetch, isRefetching, dataUpdatedAt } = useQuery({
    queryKey: ['workflows'],
    queryFn: api.getWorkflows,
    refetchInterval: 15000,
  })

  const table = useTableState({ rows: workflows, searchText, filters: FILTERS, sortAccessors: SORTS, defaultSort: DEFAULT_SORT })

  if (isError && !workflows) {
    return (
      <>
        <PageHero eyebrow="Workflows" title="Workflows unavailable." tint="red" />
        <ErrorState title="Could not load workflows." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  const openWorkflow = workflows?.find((w) => nameOf(w) === openName)
  const columns: Column<Workflow>[] = [
    {
      key: 'name',
      header: 'Name',
      sortable: true,
      render: (w) => (
        <button
          type="button"
          className="th-sort"
          aria-expanded={openName === nameOf(w)}
          aria-controls={openName === nameOf(w) ? 'workflow-detail' : undefined}
          onClick={(e) => {
            e.stopPropagation()
            setOpenName(openName === nameOf(w) ? null : nameOf(w))
          }}
        >
          <span className="faint" aria-hidden="true">{openName === nameOf(w) ? '▾' : '▸'}</span>{' '}
          <b className="mono">{nameOf(w)}</b>
        </button>
      ),
    },
    { key: 'steps', header: 'Steps', sortable: true, numeric: true, render: (w) => <span className="muted">{mergeSteps(w).length}</span> },
    { key: 'status', header: 'Status', sortable: true, render: (w) => <span className={`pill ${phaseTone(phaseOf(w))}`}>{phaseOf(w)}</span> },
    {
      key: 'progress',
      header: 'Progress',
      render: (w) => {
        const total = mergeSteps(w).length
        const done = w.status?.steps?.filter((s) => phaseGroup(s.status) === 'completed').length ?? 0
        return (
          <>
            <Progress value={done} max={total} label={`${nameOf(w)}: ${done} of ${total} steps complete`} tone={phaseGroup(phaseOf(w)) === 'failed' ? 'bad' : undefined} />
            <span className="faint num">{done}/{total}</span>
          </>
        )
      },
    },
    { key: 'duration', header: 'Duration', sortable: true, numeric: true, render: (w) => <span className="muted">{w.status?.duration ?? '—'}</span> },
    {
      key: 'started',
      header: 'Started',
      sortable: true,
      render: (w) => (
        <span className="faint" title={formatDate(w.status?.startedAt)}>
          {formatRelative(w.status?.startedAt, now)}
        </span>
      ),
    },
  ]

  const groups = workflows ? countByGroup(workflows, (w) => w.status?.phase) : undefined
  const failed = groups?.failed
  const stepsRunning = workflows?.reduce((sum, w) => sum + (w.status?.steps?.filter((s) => phaseGroup(s.status) === 'running').length ?? 0), 0)

  return (
    <>
      <PageHero eyebrow="Workflows" title="Pipelines, end to end." lede="Pipelines of jobs, scripts and webhooks that run in dependency order." />

      <div className="grid">
        <PagePulse
          updatedAt={dataUpdatedAt}
          error={isError ? errorMessage(error) : undefined}
          headline={
            failed === undefined
              ? undefined
              : failed > 0
                ? `${failed} workflow${failed === 1 ? '' : 's'} failed.`
                : `${groups?.running ?? 0} workflows running, none failed.`
          }
          tone={failed === undefined ? undefined : countTone(failed, 1)}
          figures={[
            { label: 'Active workflows', value: groups?.running },
            { label: 'Completed', value: groups?.completed },
            { label: 'Failed', value: failed, tone: failed === undefined ? undefined : countTone(failed, 1) },
            { label: 'Steps running', value: stepsRunning },
          ]}
        />

        {isError && (
          <div className="span3">
            <ErrorState title="Showing the last data received; refreshing failed." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}

        <section className="card span3">
          <p className="eyebrow">Pipelines</p>
          <h2 className="card-title">All workflows</h2>
          {isLoading ? (
            <Skeleton rows={4} />
          ) : (
            <>
              {openWorkflow && (
                <div className="card" id="workflow-detail" role="region" aria-label={`${nameOf(openWorkflow)} details`}>
                  <div className="row">
                    <p className="eyebrow">Steps: {nameOf(openWorkflow)}</p>
                    <button type="button" className="danger" onClick={() => setDeleting(nameOf(openWorkflow))}>
                      Delete workflow
                    </button>
                    <button type="button" className="btn-secondary" onClick={() => setOpenName(null)}>
                      Close details
                    </button>
                  </div>
                  <DAGVisualization key={nameOf(openWorkflow)} steps={mergeSteps(openWorkflow)} started={!!openWorkflow.status?.steps} />
                </div>
              )}
              <DataTable
                caption="Workflows"
                columns={columns}
                state={table}
                rowKey={nameOf}
                onRowClick={(w) => setOpenName(openName === nameOf(w) ? null : nameOf(w))}
                actions={
                  <>
                    <button type="button" className="primary" onClick={() => setShowCreate(true)}>
                      Create workflow
                    </button>
                    <button type="button" className="btn-refresh" onClick={() => refetch()} disabled={isRefetching} aria-busy={isRefetching}>
{isRefetching ? 'Refreshing…' : 'Refresh'}
</button>
                  </>
                }
                empty={
                  <EmptyState
                    title="No workflows yet."
                    action={
                      <button type="button" className="primary" onClick={() => setShowCreate(true)}>
                        Create your first workflow
                      </button>
                    }
                  >
                    Workflows are created here. Each step waits for the steps it depends on before it starts.
                  </EmptyState>
                }
              />
            </>
          )}
        </section>
      </div>
      {deleting && (
        <ConfirmDialog
          title={`Delete workflow ${deleting}?`}
          confirmLabel="Delete workflow"
          busy={deleteMutation.isPending}
          onCancel={() => setDeleting(null)}
          onConfirm={() => deleteMutation.mutate(deleting)}
        >
          Deleting {deleting} also stops its running jobs. This cannot be undone.
        </ConfirmDialog>
      )}
      {showCreate && <CreateWorkflowModal onClose={() => setShowCreate(false)} />}
    </>
  )
}

// --- Sub-components ---

const NOT_STARTED = 'Not started'

/** Spec steps carry type and dependsOn; status steps carry progress. Combine them by step name. */
function mergeSteps(workflow: Workflow): WorkflowStep[] {
  const spec = workflow.spec?.steps ?? []
  const status = workflow.status?.steps ?? []
  const statusByName = new Map(status.map((s) => [s.name, s]))
  const specNames = new Set(spec.map((s) => s.name))
  const merged: WorkflowStep[] = spec.map((s) => ({ ...s, ...statusByName.get(s.name), type: statusByName.get(s.name)?.type ?? s.type, dependsOn: statusByName.get(s.name)?.dependsOn ?? s.dependsOn }))
  for (const s of status) if (!specNames.has(s.name)) merged.push(s)
  return merged
}

function CreateWorkflowModal({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [spec, setSpec] = useState<SpecState>(() => initialSpecState())
  const [initialJson] = useState(spec.json)
  const [submitted, setSubmitted] = useState(false)
  const dirty = name !== '' || spec.json !== initialJson

  const createMutation = useMutation({
    mutationFn: (body: { name: string; spec: ReturnType<typeof specBody> }) => api.createWorkflow({ metadata: { name: body.name }, spec: body.spec }),
    onSuccess: (_d, vars) => {
      notify.success(`Created workflow ${vars.name}`)
      queryClient.invalidateQueries({ queryKey: ['workflows'] })
      onClose()
    },
    onError: (err) => notify.error('Could not create workflow', err),
  })

  const nameErr = nameError(name)
  const nameShown = submitted || name !== '' ? nameErr : null
  const specErr = specError(spec)

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    setSubmitted(true)
    if (nameErr || specErr) return
    createMutation.mutate({ name, spec: specBody(spec) })
  }

  return (
    <Modal className="modal-wide" title="Create workflow" onClose={onClose} dirty={dirty}>
      <form onSubmit={handleSubmit} className="stack" noValidate>
        <label className="field">
          <span>Name</span>
          <input type="text" data-autofocus value={name} onChange={(e) => setName(e.target.value)} placeholder="my-pipeline" aria-invalid={!!nameShown} aria-describedby="wf-name-msg" autoComplete="off" />
          <span id="wf-name-msg" className={nameShown ? 'warning' : 'faint'}>
            {nameShown ?? 'Lowercase letters, digits and hyphens.'}
          </span>
        </label>
        <StepEditor state={spec} onChange={setSpec} showErrors={submitted} />
        {submitted && specErr && (
          <p className="warning" role="alert">
            {specErr}
          </p>
        )}
        {createMutation.isError && (
          <p className="warning" role="alert">
            {errorMessage(createMutation.error)}
          </p>
        )}
        <div className="toolbar">
          <button type="button" className="btn-secondary" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={createMutation.isPending}>
            {createMutation.isPending ? 'Creating…' : 'Create workflow'}
          </button>
        </div>
      </form>
    </Modal>
  )
}

function DAGVisualization({ steps, started }: { steps: WorkflowStep[]; started: boolean }) {
  const [selectedName, setSelectedName] = useState<string | null>(null)
  const stepStatus = (s: WorkflowStep) => (started && s.status ? s.status : NOT_STARTED)
  const selectedStep = steps.find((s) => s.name === selectedName)

  if (steps.length === 0) {
    return <EmptyState title="No steps defined.">The workflow spec lists no steps, so there is nothing to run.</EmptyState>
  }

  const layers = dagLayers(steps)

  return (
    <div className="stack">
      <ol className="sr-only" aria-label="Execution order">
        {describeSteps(steps, (s) => stepStatus(s)).map((line) => (
          <li key={line}>{line}</li>
        ))}
      </ol>
      <div className="row" role="group" aria-label="Workflow steps, left to right by dependency">
        {layers.map((layer, i) => (
          <div key={i} className="row">
            {i > 0 && <span className="faint" aria-hidden="true">→</span>}
            <div className="stack">
              {layer.map((step) => {
                const isSelected = selectedName === step.name
                const status = stepStatus(step)
                return (
                  <button
                    key={step.name}
                    type="button"
                    className={isSelected ? 'primary' : 'btn-secondary'}
                    aria-pressed={isSelected}
                    onClick={() => setSelectedName(isSelected ? null : step.name)}
                  >
                    <span className="mono">{step.name}</span> <span className={`pill ${phaseTone(status)}`}>{status}</span>
                  </button>
                )
              })}
            </div>
          </div>
        ))}
      </div>

      {selectedStep && (
        <div className="stack">
          <div className="formgrid">
            <div>
              <div className="faint">Step name</div>
              <b className="mono">{selectedStep.name}</b>
            </div>
            <div>
              <div className="faint">Type</div>
              <span className="muted">{selectedStep.type ?? '—'}</span>
            </div>
            <div>
              <div className="faint">Depends on</div>
              <span className="muted mono">{selectedStep.dependsOn && selectedStep.dependsOn.length > 0 ? selectedStep.dependsOn.join(', ') : 'Nothing (starts first)'}</span>
            </div>
            <div>
              <div className="faint">Status</div>
              <span className={`pill ${phaseTone(stepStatus(selectedStep))}`}>{stepStatus(selectedStep)}</span>
            </div>
            <div>
              <div className="faint">Duration</div>
              <span className="muted num">{selectedStep.duration ?? '—'}</span>
            </div>
          </div>
          {selectedStep.jobRef && (
            <Link to={`/jobs/${encodeURIComponent(selectedStep.jobRef)}`} className="card-link">
              View job logs: {selectedStep.jobRef} ›
            </Link>
          )}
        </div>
      )}
    </div>
  )
}
