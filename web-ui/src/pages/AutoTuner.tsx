// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useId, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import type { AutoTunerJob } from '@/lib/api'
import { errorMessage } from '@/lib/errors'
import { formatDate, formatRelative } from '@/lib/format'
import { phaseTone } from '@/lib/phase'
import { notify } from '@/lib/notify'
import { fieldAria, intError, nameError, parseIntStrict } from '@/lib/forms'
import { useNow } from '@/lib/useNow'
import { bestAcross, formatMetric, metricNameOf } from '@/lib/tuner'
import { initialSpaceState, spaceError, spaceJson, type ParamSpaceState } from '@/lib/paramSpace'
import type { FilterDef, SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import ConfirmDialog from '@/components/ConfirmDialog'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import Modal from '@/components/Modal'
import Progress from '@/components/Progress'
import DataTable, { type Column } from '@/components/DataTable'
import ParamSpaceEditor from '@/components/ParamSpaceEditor'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { TableCaption } from '@/components/TableCaption'

/** The gateway now also returns spec.direction and spec.metricName; not in the shared type yet. */
type Tuner = AutoTunerJob & { spec?: { direction?: 'maximize' | 'minimize'; metricName?: string } }

const ALGORITHM_LABELS: Record<string, string> = { grid: 'Grid', random: 'Random', bayesian: 'Bayesian', asha: 'ASHA' }
const algorithmLabel = (a?: string) => (a ? (ALGORITHM_LABELS[a.toLowerCase()] ?? a) : '—')

const nameOf = (t: Tuner) => t.metadata?.name || 'unknown'
const phaseOf = (t: Tuner) => t.status?.phase || 'Pending'

const FILTERS: FilterDef<Tuner>[] = [
  { name: 'status', label: 'Status', get: phaseOf },
  { name: 'algorithm', label: 'Algorithm', get: (t) => (t.spec?.algorithm ? algorithmLabel(t.spec.algorithm) : undefined) },
]
const SORTS: Record<string, SortAccessor<Tuner>> = {
  name: nameOf,
  algorithm: (t) => t.spec?.algorithm,
  trials: (t) => t.status?.trialsCompleted ?? 0,
  best: (t) => t.status?.bestMetricValue ?? undefined,
  status: phaseOf,
  created: (t) => (t.metadata?.creationTimestamp ? Date.parse(t.metadata.creationTimestamp) : undefined),
}
const searchText = (t: Tuner) => `${nameOf(t)} ${phaseOf(t)} ${algorithmLabel(t.spec?.algorithm)} ${metricNameOf(t) ?? ''}`
const DEFAULT_SORT = { key: 'created', dir: 'desc' as const }

export default function AutoTuner() {
  useDocumentTitle('Auto Tuner')
  const [showCreateForm, setShowCreateForm] = useState(false)
  const [openName, setOpenName] = useState<string | null>(null)
  const queryClient = useQueryClient()
  const [deleting, setDeleting] = useState<string | null>(null)
  const deleteMutation = useMutation({
    mutationFn: (name: string) => api.deleteTuner(name),
    onSuccess: (_d, name) => {
      notify.success(`Deleted tuner ${name}`)
      setDeleting(null)
      if (openName === name) setOpenName(null)
      return queryClient.invalidateQueries({ queryKey: ['tuners'] })
    },
    onError: (err, name) => {
      notify.error(`Could not delete ${name}`, err)
      setDeleting(null)
    },
  })
  const now = useNow(30000)

  const { data, isLoading, isError, error, refetch, isRefetching, dataUpdatedAt } = useQuery({
    queryKey: ['tuners'],
    queryFn: api.getTuners,
    refetchInterval: 15000,
  })
  const tuners = data as Tuner[] | undefined
  const table = useTableState({ rows: tuners, searchText, filters: FILTERS, sortAccessors: SORTS, defaultSort: DEFAULT_SORT })

  if (isError && !tuners) {
    return (
      <>
        <PageHero eyebrow="Auto Tuner" title="Tuners unavailable." tint="red" />
        <ErrorState title="Could not load auto tuners." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  const openTuner = tuners?.find((t) => nameOf(t) === openName)
  const toggleOpen = (t: Tuner) => setOpenName(openName === nameOf(t) ? null : nameOf(t))
  const columns: Column<Tuner>[] = [
    {
      key: 'name',
      header: 'Name',
      sortable: true,
      render: (t) => (
        <button
          type="button"
          className="th-sort"
          aria-expanded={openName === nameOf(t)}
          aria-controls={openName === nameOf(t) ? 'tuner-detail' : undefined}
          onClick={(e) => {
            e.stopPropagation()
            toggleOpen(t)
          }}
        >
          <span className="faint" aria-hidden="true">{openName === nameOf(t) ? '▾' : '▸'}</span>{' '}
          <b className="mono">{nameOf(t)}</b>
        </button>
      ),
    },
    { key: 'algorithm', header: 'Algorithm', sortable: true, render: (t) => <span className="pill">{algorithmLabel(t.spec?.algorithm)}</span> },
    {
      key: 'trials',
      header: 'Trials',
      sortable: true,
      numeric: true,
      render: (t) => {
        const done = t.status?.trialsCompleted ?? 0
        const max = t.spec?.maxTrials ?? 0
        return (
          <>
            <span className="muted num">{done}/{max}</span>
            <Progress value={done} max={max} label={`${nameOf(t)}: ${done} of ${max} trials complete`} />
          </>
        )
      },
    },
    {
      key: 'best',
      header: 'Best metric',
      sortable: true,
      numeric: true,
      render: (t) => {
        const v = t.status?.bestMetricValue
        const metric = metricNameOf(t)
        return v !== undefined && v !== null ? (
          <>
            <b>{formatMetric(v, metric)}</b>
            {metric && <div className="faint">{metric}{t.spec?.direction ? ` (${t.spec.direction === 'minimize' ? 'lower' : 'higher'} is better)` : ''}</div>}
          </>
        ) : (
          <span className="faint">—</span>
        )
      },
    },
    { key: 'status', header: 'Status', sortable: true, render: (t) => <span className={`pill ${phaseTone(phaseOf(t))}`}>{phaseOf(t)}</span> },
    {
      key: 'created',
      header: 'Created',
      sortable: true,
      render: (t) => (
        <span className="faint" title={formatDate(t.metadata?.creationTimestamp)}>
          {formatRelative(t.metadata?.creationTimestamp, now)}
        </span>
      ),
    },
  ]

  const active = tuners?.filter((t) => t.status?.phase === 'Running').length
  const trialsRunning = tuners?.reduce((sum, t) => sum + (t.status?.trialsRunning ?? 0), 0)
  const trialsCompleted = tuners?.reduce((sum, t) => sum + (t.status?.trialsCompleted ?? 0), 0)
  const best = bestAcross(tuners)
  const bestLabel = best.mixed ? 'Best metric' : best.metricName ? `Best ${best.metricName}` : 'Best metric'
  const bestValue = !tuners ? undefined : best.mixed ? 'Varies by tuner' : formatMetric(best.value, best.metricName)

  return (
    <>
      <PageHero eyebrow="Auto Tuner" title="Find the best hyperparameters." lede="Hyperparameter searches that run many trials of your training image and track the best." />

      <div className="grid">
        <PagePulse
          updatedAt={dataUpdatedAt}
          error={isError ? errorMessage(error) : undefined}
          headline={tuners ? `${active} of ${tuners.length} tuners running ${trialsRunning} trials.` : undefined}
          figures={[
            { label: 'Active tuners', value: active },
            { label: 'Trials running', value: trialsRunning },
            { label: bestLabel, value: bestValue },
            { label: 'Trials completed', value: trialsCompleted },
          ]}
        />

        {isError && (
          <div className="span3">
            <ErrorState title="Showing the last data received; refreshing failed." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}

        <section className="card span3">
          <p className="eyebrow">Tuning</p>
          <h2 className="card-title">All tuners</h2>
          {isLoading ? (
            <Skeleton rows={4} />
          ) : (
            <>
              {openTuner && (
                <div className="card" id="tuner-detail" role="region" aria-label={`${nameOf(openTuner)} trials`}>
                  <div className="row">
                    <p className="eyebrow">Trials: {nameOf(openTuner)}</p>
                    <button type="button" className="danger" onClick={() => setDeleting(nameOf(openTuner))}>
                      Delete tuner
                    </button>
                    <button type="button" className="btn-secondary" onClick={() => setOpenName(null)}>
                      Close details
                    </button>
                  </div>
                  <TunerDetail key={nameOf(openTuner)} tuner={openTuner} now={now} />
                </div>
              )}
              <DataTable
                caption="Auto tuners"
                columns={columns}
                state={table}
                rowKey={nameOf}
                onRowClick={toggleOpen}
                actions={
                  <>
                    <button type="button" className="primary" onClick={() => setShowCreateForm(true)}>
                      New tuner
                    </button>
                    <button type="button" className="btn-refresh" onClick={() => refetch()} disabled={isRefetching} aria-busy={isRefetching}>
{isRefetching ? 'Refreshing…' : 'Refresh'}
</button>
                  </>
                }
                empty={
                  <EmptyState
                    title="No tuning jobs yet."
                    action={
                      <button type="button" className="primary" onClick={() => setShowCreateForm(true)}>
                        Create your first tuner
                      </button>
                    }
                  >
                    Tuners are created here. Each runs trials of your training image and reports the best metric.
                  </EmptyState>
                }
              />
            </>
          )}
        </section>
      </div>

      {deleting && (
        <ConfirmDialog
          title={`Delete tuner ${deleting}?`}
          confirmLabel="Delete tuner"
          busy={deleteMutation.isPending}
          onCancel={() => setDeleting(null)}
          onConfirm={() => deleteMutation.mutate(deleting)}
        >
          Deleting {deleting} stops the search and deletes its trial jobs. This cannot be undone.
        </ConfirmDialog>
      )}
      {showCreateForm && <CreateTunerModal onClose={() => setShowCreateForm(false)} />}
    </>
  )
}

// --- Sub-components ---

function TunerDetail({ tuner, now }: { tuner: Tuner; now: number }) {
  const name = tuner.metadata?.name || 'unknown'
  const metricName = metricNameOf(tuner)

  const { data: trials, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ['tunerTrials', name],
    queryFn: () => api.getTunerTrials(name),
    // Poll while anything is still running so the table does not go stale.
    refetchInterval: (query) => (query.state.data?.some((t) => t.status === 'Running') ? 5000 : false),
  })

  // The best trial is decided by the operator (which knows the direction); never recompute it here.
  const bestId = tuner.status?.bestTrialId
  const bestTrial = bestId ? trials?.find((t) => t.trialId === bestId) : undefined
  const bestValue = tuner.status?.bestMetricValue ?? bestTrial?.metricValue

  return (
    <div className="stack">
      {bestId && (
        <div>
          <div className="row">
            <b>
              Best trial: <span className="mono">{bestId}</span>
            </b>
            <b>{formatMetric(bestValue, metricName)}</b>
          </div>
          {bestTrial?.parameters && Object.keys(bestTrial.parameters).length > 0 && (
            <div className="row">
              {Object.entries(bestTrial.parameters).map(([key, val]) => (
                <span key={key} className="pill mono">
                  {key}={String(val)}
                </span>
              ))}
            </div>
          )}
        </div>
      )}

      <div>
        <p className="eyebrow">Trial results</p>
        {isLoading ? (
          <Skeleton rows={3} />
        ) : isError ? (
          <ErrorState title="Could not load trials." error={error} onRetry={() => refetch()} retrying={isRefetching} />
        ) : (trials ?? []).length === 0 ? (
          <EmptyState title="No trials yet.">Trials are reported by the tuner as it runs them, so they appear here once it starts.</EmptyState>
        ) : (
          <div className="table-wrap">
            <table>
              <TableCaption>{`Trials for ${name}`}</TableCaption>
              <thead>
                <tr>
                  <th scope="col">Trial ID</th>
                  <th scope="col">Parameters</th>
                  <th scope="col" className="num">{metricName ?? 'Metric'}</th>
                  <th scope="col">Status</th>
                  <th scope="col" className="num">Duration</th>
                </tr>
              </thead>
              <tbody>
                {(trials ?? []).map((trial) => (
                  <tr key={trial.trialId}>
                    <td className="mono">
                      {trial.trialId}
                      {trial.trialId === bestId && (
                        <>
                          {' '}
                          <span className="pill ok">best</span>
                        </>
                      )}
                    </td>
                    <td>
                      <div className="row">
                        {trial.parameters &&
                          Object.entries(trial.parameters).map(([k, v]) => (
                            <span key={k} className="pill mono">
                              {k}={String(v)}
                            </span>
                          ))}
                      </div>
                    </td>
                    <td className="num">
                      <b>{formatMetric(trial.metricValue, metricName)}</b>
                    </td>
                    <td><span className={`pill ${phaseTone(trial.status)}`}>{trial.status ?? '—'}</span></td>
                    <td className="faint num">{trial.duration ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {tuner.metadata?.creationTimestamp && (
          <p className="faint" title={formatDate(tuner.metadata.creationTimestamp)}>
            Created {formatRelative(tuner.metadata.creationTimestamp, now)}
          </p>
        )}
      </div>
    </div>
  )
}

const INITIAL = {
  name: '',
  algorithm: 'Bayesian',
  objectiveMetric: 'accuracy',
  direction: 'maximize' as 'maximize' | 'minimize',
  image: '',
  gpus: '1',
  maxTrials: '20',
  parallelism: '0',
  maxEpochs: '10',
  reductionFactor: '',
  minResource: '',
}

function CreateTunerModal({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient()
  const [form, setForm] = useState(INITIAL)
  const [space, setSpace] = useState<ParamSpaceState>(() => initialSpaceState())
  const [initialSpace] = useState(space.json)
  const uid = useId()
  const [touched, setTouched] = useState(false)
  const set = (patch: Partial<typeof INITIAL>) => setForm((f) => ({ ...f, ...patch }))
  const dirty = JSON.stringify(form) !== JSON.stringify(INITIAL) || space.json !== initialSpace
  const isAsha = form.algorithm === 'ASHA'

  const createMutation = useMutation({
    mutationFn: (body: Parameters<typeof api.createTuner>[0]) => api.createTuner(body),
    onSuccess: (_d, vars) => {
      notify.success(`Created tuner ${vars.name}`)
      queryClient.invalidateQueries({ queryKey: ['tuners'] })
      onClose()
    },
    onError: (err) => notify.error('Could not create tuner', err),
  })

  const optionalInt = (raw: string, min: number, max: number) => (raw.trim() === '' ? null : intError(raw, min, max))
  const errors = {
    name: nameError(form.name),
    objective: /^[A-Za-z0-9_./-]+$/.test(form.objectiveMetric) && form.objectiveMetric.length <= 128 ? null : 'Use letters, digits and _ . / - only.',
    image: form.image.trim() === '' ? 'Enter the container image each trial runs.' : /\s/.test(form.image.trim()) ? 'The image cannot contain spaces.' : null,
    gpus: intError(form.gpus, 0, 1024),
    maxTrials: intError(form.maxTrials, 1, 10000),
    parallelism: intError(form.parallelism, 0, 256),
    maxEpochs: isAsha ? intError(form.maxEpochs, 1, 100000) : null,
    reductionFactor: isAsha ? optionalInt(form.reductionFactor, 2, 100) : null,
    minResource: isAsha ? optionalInt(form.minResource, 1, 100000) : null,
    space: spaceError(space),
  }
  const invalid = Object.values(errors).some(Boolean)
  const show = (e: string | null) => (touched ? e : null)
  const nameShown = touched || form.name !== '' ? errors.name : null
  const msg = (id: string, err: string | null | undefined, hint?: string) =>
    err || hint ? (
      <span id={`${uid}-${id}-msg`} className={err ? 'warning' : 'faint'}>
        {err ?? hint}
      </span>
    ) : null

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    setTouched(true)
    if (invalid) return
    const int = (raw: string) => parseIntStrict(raw) as number
    const body = {
      name: form.name,
      algorithm: form.algorithm,
      objectiveMetric: form.objectiveMetric,
      direction: form.direction,
      maxTrials: int(form.maxTrials),
      parallelism: int(form.parallelism),
      parameterSpace: spaceJson(space),
      jobTemplate: { type: 'training' as const, image: form.image.trim(), gpus: int(form.gpus) },
      ...(isAsha
        ? {
            ashaConfig: {
              maxEpochs: int(form.maxEpochs),
              ...(form.reductionFactor.trim() ? { reductionFactor: int(form.reductionFactor) } : {}),
              ...(form.minResource.trim() ? { minResource: int(form.minResource) } : {}),
            },
          }
        : {}),
    }
    createMutation.mutate(body)
  }

  const num = (label: string, key: keyof typeof INITIAL, err: string | null, hint?: string, min = 0, max?: number, required = true) => {
    const shown = show(err)
    return (
      <label className="field">
        {label}
        <input type="number" min={min} max={max} required={false} value={form[key]} onChange={(e) => set({ [key]: e.target.value })} aria-required={required} {...fieldAria(`${uid}-${key}`, shown, !!hint)} />
        {msg(key, shown, hint)}
      </label>
    )
  }

  return (
    <Modal className="modal-wide" title="Create tuner" onClose={onClose} dirty={dirty}>
      <form onSubmit={handleSubmit} className="stack" noValidate>
        <label className="field">
          Name
          <input type="text" data-autofocus value={form.name} onChange={(e) => set({ name: e.target.value })} placeholder="my-tuning-job" autoComplete="off" spellCheck={false} {...fieldAria(`${uid}-name`, nameShown, true)} />
          {msg('name', nameShown, 'Lowercase letters, digits and hyphens.')}
        </label>

        <div className="formgrid">
          <label className="field">
            Algorithm
            <select value={form.algorithm} onChange={(e) => set({ algorithm: e.target.value })}>
              <option value="Grid">Grid search</option>
              <option value="Random">Random search</option>
              <option value="Bayesian">Bayesian optimization</option>
              <option value="ASHA">ASHA</option>
            </select>
          </label>
          <label className="field">
            Objective metric
            <input type="text" value={form.objectiveMetric} onChange={(e) => set({ objectiveMetric: e.target.value })} placeholder="accuracy" spellCheck={false} {...fieldAria(`${uid}-objective`, show(errors.objective))} />
            {msg('objective', show(errors.objective))}
          </label>
          <label className="field">
            Goal
            <select value={form.direction} onChange={(e) => set({ direction: e.target.value as 'maximize' | 'minimize' })}>
              <option value="maximize">Maximize the metric</option>
              <option value="minimize">Minimize the metric</option>
            </select>
          </label>
        </div>

        <div className="formgrid">
          <label className="field">
            Trial image
            <input type="text" className="mono" value={form.image} onChange={(e) => set({ image: e.target.value })} placeholder="registry.example.com/train:latest" autoComplete="off" spellCheck={false} aria-required="true" {...fieldAria(`${uid}-image`, show(errors.image), true)} />
            {msg('image', show(errors.image), 'Required. The container each trial runs; it must report the objective metric.')}
          </label>
          {num('GPUs per trial', 'gpus', errors.gpus, undefined, 0, 1024)}
        </div>

        <div className="formgrid">
          {num('Max trials', 'maxTrials', errors.maxTrials, 'Total trials to run (1 to 10000).', 1, 10000)}
          {num('Parallelism', 'parallelism', errors.parallelism, 'Trials at once. 0 lets the operator decide.', 0, 256)}
        </div>

        {isAsha && (
          <div className="formgrid">
            {num('Max epochs (ASHA)', 'maxEpochs', errors.maxEpochs, 'Longest a single trial may train.', 1, 100000)}
            {num('Reduction factor', 'reductionFactor', errors.reductionFactor, 'Optional, 2 or more. Blank uses the default.', 2, 100, false)}
            {num('Min resource', 'minResource', errors.minResource, 'Optional. Epochs every trial gets before pruning.', 1, 100000, false)}
          </div>
        )}

        <ParamSpaceEditor state={space} onChange={setSpace} showErrors={touched} />
        {touched && !space.advanced && errors.space && (
          <p className="warning" role="alert">
            {errors.space}
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
            {createMutation.isPending ? 'Creating…' : 'Create tuner'}
          </button>
        </div>
      </form>
    </Modal>
  )
}
