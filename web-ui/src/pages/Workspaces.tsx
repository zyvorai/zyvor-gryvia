import { useId, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import type { Workspace } from '@/lib/api'
import { errorMessage } from '@/lib/errors'
import { formatDate, formatRelative } from '@/lib/format'
import { phaseTone } from '@/lib/phase'
import { notify } from '@/lib/notify'
import { buildIdleTimeout, buildStorage, fieldAria, intError, nameError, parseIntStrict } from '@/lib/forms'
import { useNow } from '@/lib/useNow'
import type { FilterDef, SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import Modal from '@/components/Modal'
import ConfirmDialog from '@/components/ConfirmDialog'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import CopyButton from '@/components/CopyButton'
import { SrOnly } from '@/components/TableCaption'

/** The gateway also returns status.message; it is not in the shared type yet. */
type WorkspaceView = Workspace & { status?: { message?: string } }

const nameOf = (w: WorkspaceView) => w.metadata?.name || 'unknown'
const phaseOf = (w: WorkspaceView) => w.status?.phase || 'Pending'
const TYPE_LABEL: Record<string, string> = { jupyter: 'Jupyter', vscode: 'VS Code' }
const typeOf = (w: WorkspaceView) => w.spec?.type || 'jupyter'

const FILTERS: FilterDef<WorkspaceView>[] = [
  { name: 'status', label: 'Status', get: phaseOf },
  { name: 'type', label: 'Type', get: typeOf },
]
const SORTS: Record<string, SortAccessor<WorkspaceView>> = {
  name: nameOf,
  status: phaseOf,
  gpus: (w) => w.spec?.gpuCount,
  created: (w) => (w.metadata?.creationTimestamp ? Date.parse(w.metadata.creationTimestamp) : undefined),
}
const SORT_LABELS: [string, string][] = [
  ['name', 'Name'],
  ['status', 'Status'],
  ['gpus', 'GPUs'],
  ['created', 'Created'],
]
const searchText = (w: WorkspaceView) => `${nameOf(w)} ${phaseOf(w)} ${TYPE_LABEL[typeOf(w)] ?? typeOf(w)} ${w.spec?.gpuType ?? ''}`
const DEFAULT_SORT = { key: 'name', dir: 'asc' as const }
const PAGE_SIZE = 12

export default function Workspaces() {
  useDocumentTitle('Workspaces')
  const queryClient = useQueryClient()
  const [showCreateForm, setShowCreateForm] = useState(false)
  const [deleting, setDeleting] = useState<string | null>(null)
  const now = useNow(30000)

  const { data: workspaces, isLoading, isError, error, refetch, isRefetching, dataUpdatedAt } = useQuery({
    queryKey: ['workspaces'],
    queryFn: api.getWorkspaces,
    refetchInterval: 15000,
  })

  const refresh = () => queryClient.invalidateQueries({ queryKey: ['workspaces'] })

  const pauseMutation = useMutation({
    mutationFn: (name: string) => api.pauseWorkspace(name),
    onSuccess: (_d, name) => {
      notify.success(`Paused ${name}`)
      return refresh()
    },
    onError: (err, name) => notify.error(`Could not pause ${name}`, err),
  })

  const resumeMutation = useMutation({
    mutationFn: (name: string) => api.resumeWorkspace(name),
    onSuccess: (_d, name) => {
      notify.success(`Resuming ${name}`)
      return refresh()
    },
    onError: (err, name) => notify.error(`Could not resume ${name}`, err),
  })

  const deleteMutation = useMutation({
    mutationFn: (name: string) => api.deleteWorkspace(name),
    onSuccess: (_d, name) => {
      notify.success(`Deleted ${name}`)
      setDeleting(null)
      return refresh()
    },
    onError: (err, name) => {
      notify.error(`Could not delete ${name}`, err)
      setDeleting(null)
    },
  })

  const table = useTableState({ rows: workspaces as WorkspaceView[] | undefined, searchText, filters: FILTERS, sortAccessors: SORTS, defaultSort: DEFAULT_SORT, pageSize: PAGE_SIZE })

  if (isError && !workspaces) {
    return (
      <>
        <PageHero eyebrow="Workspaces" title="Workspaces unavailable." tint="red" />
        <ErrorState title="Could not load workspaces." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  const list = workspaces as WorkspaceView[] | undefined
  const loaded = !!list
  const count = (phase: string) => list?.filter((w) => w.status?.phase === phase).length
  const active = count('Running')
  const totalGPUs = list?.reduce((sum, w) => sum + (w.spec?.gpuCount ?? 0), 0)
  const deletingWs = list?.find((w) => w.metadata?.name === deleting)

  return (
    <>
      <PageHero eyebrow="Workspaces" title="Interactive GPU environments." lede="Jupyter and VS Code environments with GPUs attached, read from the cluster every 15 seconds." />

      <div className="grid">
        <PagePulse
          updatedAt={dataUpdatedAt}
          error={isError ? errorMessage(error) : undefined}
          headline={loaded ? `${active} of ${list.length} workspaces running on ${totalGPUs} GPUs.` : undefined}
          figures={[
            { label: 'Active workspaces', value: active },
            { label: 'GPUs allocated', value: totalGPUs },
            { label: 'Idle', value: count('Idle') },
            { label: 'Paused', value: count('Paused') },
          ]}
        />

        {isError && (
          <div className="span3">
            <ErrorState title="Showing the last data received; refreshing failed." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}

        <section className="card span3">
          <p className="eyebrow">Browse</p>
          <h2 className="card-title">All workspaces</h2>
          <div className="toolbar" role="search">
            <label>
              Search
              <input type="search" value={table.search} onChange={(e) => table.setSearch(e.target.value)} placeholder="Search…" />
            </label>
            {table.filterDefs.map((def) => (
              <label key={def.name}>
                {def.label}
                <select value={table.filterValues[def.name] ?? ''} onChange={(e) => table.setFilter(def.name, e.target.value)}>
                  <option value="">All</option>
                  {table.filterOptionsFor(def).map((o) => (
                    <option key={o} value={o}>
                      {def.name === 'type' ? (TYPE_LABEL[o] ?? o) : o}
                    </option>
                  ))}
                </select>
              </label>
            ))}
            <label>
              Sort by
              <select value={table.sortKey ?? DEFAULT_SORT.key} onChange={(e) => e.target.value !== table.sortKey && table.toggleSort(e.target.value)}>
                {SORT_LABELS.map(([key, label]) => (
                  <option key={key} value={key}>
                    {label}
                  </option>
                ))}
              </select>
            </label>
            <button type="button" className="btn-secondary" onClick={() => table.toggleSort(table.sortKey ?? DEFAULT_SORT.key)} aria-label={`Sort order: ${table.sortDir === 'asc' ? 'ascending' : 'descending'}. Reverse`}>
              {table.sortDir === 'asc' ? '▲ Ascending' : '▼ Descending'}
            </button>
            {table.isFiltered && (
              <button type="button" className="btn-secondary" onClick={table.reset}>
                Clear filters
              </button>
            )}
            <button type="button" className="primary" onClick={() => setShowCreateForm(true)}>
              New workspace
            </button>
            <button type="button" className="btn-refresh" onClick={() => refetch()} disabled={isRefetching} aria-busy={isRefetching}>
{isRefetching ? 'Refreshing…' : 'Refresh'}
</button>
          </div>
        </section>

        {isLoading ? (
          <section className="card span3">
            <Skeleton rows={3} />
          </section>
        ) : loaded && list.length === 0 ? (
          <section className="card span3">
            <EmptyState
              title="No workspaces yet."
              action={
                <button type="button" className="primary" onClick={() => setShowCreateForm(true)}>
                  Create your first workspace
                </button>
              }
            >
              A workspace is a Jupyter or VS Code environment with GPUs attached and a persistent volume.
            </EmptyState>
          </section>
        ) : table.filteredCount === 0 ? (
          <section className="card span3">
            <EmptyState
              title="Nothing matches these filters."
              action={
                <button type="button" className="btn-secondary" onClick={table.reset}>
                  Clear filters
                </button>
              }
            />
          </section>
        ) : (
          <>
            {table.pageRows.map((ws) => {
              const name = nameOf(ws)
              return (
                <WorkspaceCard
                  key={name}
                  workspace={ws}
                  now={now}
                  pausing={pauseMutation.isPending && pauseMutation.variables === name}
                  resuming={resumeMutation.isPending && resumeMutation.variables === name}
                  onPause={() => pauseMutation.mutate(name)}
                  onResume={() => resumeMutation.mutate(name)}
                  onDelete={() => setDeleting(name)}
                />
              )
            })}
            <div className="toolbar span3">
              <span className="faint" role="status">
                Showing {table.from}–{table.to} of {table.filteredCount}
                {table.filteredCount !== table.total ? ` (${table.total} total)` : ''}
              </span>
              {table.pageCount > 1 && (
                <nav aria-label="Pagination" className="toolbar">
                  <button type="button" className="btn-secondary" disabled={table.page <= 1} onClick={() => table.setPage(table.page - 1)}>
                    Previous
                  </button>
                  <span className="faint">
                    Page {table.page} of {table.pageCount}
                  </span>
                  <button type="button" className="btn-secondary" disabled={table.page >= table.pageCount} onClick={() => table.setPage(table.page + 1)}>
                    Next
                  </button>
                </nav>
              )}
            </div>
          </>
        )}
      </div>

      {deleting && (
        <ConfirmDialog
          title={`Delete workspace ${deleting}?`}
          confirmLabel="Delete workspace"
          busy={deleteMutation.isPending}
          onCancel={() => setDeleting(null)}
          onConfirm={() => deleteMutation.mutate(deleting)}
        >
          Deleting {deleting} also deletes its {deletingWs?.spec?.storageSize ?? 'storage'} volume. This cannot be undone.
        </ConfirmDialog>
      )}

      {showCreateForm && <CreateWorkspaceModal onClose={() => setShowCreateForm(false)} />}
    </>
  )
}

/** status.url is a gateway path (/ws/<namespace>/<name>/) when the workspace proxy is on. */
function isGatewayPath(url: string): boolean {
  try {
    return new URL(url, window.location.href).pathname.startsWith('/ws/')
  } catch {
    return false
  }
}

/** The tab is opened before the request so popup blockers treat it as part of the click. */
async function openThroughGateway(name: string) {
  const tab = window.open('', '_blank')
  if (tab) tab.opener = null
  try {
    const { url } = await api.openWorkspace(name)
    if (tab) tab.location.href = url
    else window.location.href = url
  } catch (err) {
    tab?.close()
    notify.error(`Could not open ${name}`, err)
  }
}

// --- Sub-components ---

function WorkspaceCard({ workspace, now, onPause, onResume, onDelete, pausing, resuming }: {
  workspace: WorkspaceView
  now: number
  onPause: () => void
  onResume: () => void
  onDelete: () => void
  pausing: boolean
  resuming: boolean
}) {
  const name = workspace.metadata?.name || 'unknown'
  // A workspace has no phase until the operator reconciles it: that is provisioning, not unknown.
  const rawPhase = workspace.status?.phase
  const phase = rawPhase || 'Pending'
  const provisioning = !rawPhase
  const wsType = workspace.spec?.type || 'jupyter'
  const url = workspace.status?.url
  const canOpen = (phase === 'Running' || phase === 'Idle') && !!url
  const proxied = !!url && isGatewayPath(url)
  const gpuCount = workspace.spec?.gpuCount
  const gpuText = gpuCount === undefined || gpuCount === null ? '—' : gpuCount === 0 ? 'No GPU' : `${workspace.spec?.gpuType ?? 'GPU'} ×${gpuCount}`

  return (
    <section className="card" aria-labelledby={`ws-${name}-title`}>
      <p className="eyebrow">{TYPE_LABEL[wsType] || wsType}</p>
      <h2 className="card-title mono" id={`ws-${name}-title`}>
        {name}
      </h2>
      <p>
        <span className={`pill ${phaseTone(phase)}`}>{phase}</span>
      </p>
      {provisioning && <p className="muted" role="status">Provisioning: the operator has not reported a phase yet.</p>}
      {phase === 'Failed' && workspace.status?.message && (
        <p className="warning" role="alert">
          {workspace.status.message}
        </p>
      )}

      <div className="stack">
        <span className="muted">GPUs: {gpuText}</span>
        <span className="muted">
          Storage: <span className="num">{workspace.spec?.storageSize ?? '—'}</span>
        </span>
        <span className="muted">
          Uptime: <span className="num">{workspace.status?.uptime ?? '—'}</span>
        </span>
        {workspace.status?.lastActivity && (
          <span className="faint" title={formatDate(workspace.status.lastActivity)}>
            Last active: {formatRelative(workspace.status.lastActivity, now)}
          </span>
        )}
      </div>

      <div className="toolbar">
        {canOpen && proxied && (
          <button type="button" className="btn-secondary" onClick={() => openThroughGateway(name)}>
            Open <span aria-hidden="true">↗</span>
            <SrOnly> {name} in a new tab</SrOnly>
          </button>
        )}
        {canOpen && !proxied && (
          <>
            <a href={url} target="_blank" rel="noopener noreferrer" className="buttonlike btn-secondary">
              Open <span aria-hidden="true">↗</span>
              <SrOnly> {name} in a new tab</SrOnly>
            </a>
            <CopyButton value={url} label={`${name} URL`} />
          </>
        )}
        {phase === 'Running' || phase === 'Idle' ? (
          <button type="button" className="btn-secondary" onClick={onPause} disabled={pausing}>
            {pausing ? 'Pausing…' : 'Pause'}
          </button>
        ) : phase === 'Paused' ? (
          <button type="button" className="btn-secondary" onClick={onResume} disabled={resuming}>
            {resuming ? 'Resuming…' : 'Resume'}
          </button>
        ) : null}
        <button type="button" className="danger" onClick={onDelete} aria-label={`Delete workspace ${name}`}>
          Delete
        </button>
      </div>
    </section>
  )
}

const INITIAL = {
  name: '',
  type: 'jupyter',
  gpuCount: '1',
  gpuType: 'A100-80G',
  storageAmount: '50',
  storageUnit: 'Gi',
  idleAmount: '30',
  idleUnit: 'm',
}

function CreateWorkspaceModal({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient()
  const [form, setForm] = useState(INITIAL)
  const uid = useId()
  const [touched, setTouched] = useState(false)
  const set = (patch: Partial<typeof INITIAL>) => setForm((f) => ({ ...f, ...patch }))
  const dirty = JSON.stringify(form) !== JSON.stringify(INITIAL)

  const createMutation = useMutation({
    mutationFn: (data: Parameters<typeof api.createWorkspace>[0]) => api.createWorkspace(data),
    onSuccess: (created, vars) => {
      notify.success(`Created workspace ${vars.name}`)
      // Show it immediately (with no phase yet) instead of waiting for the next poll.
      const entry: Workspace = created?.metadata?.name ? created : { metadata: { name: vars.name }, spec: { ...vars } }
      queryClient.setQueryData<Workspace[]>(['workspaces'], (old) => [...(old ?? []).filter((w) => w.metadata?.name !== vars.name), entry])
      queryClient.invalidateQueries({ queryKey: ['workspaces'] })
      onClose()
    },
    onError: (err) => notify.error('Could not create workspace', err),
  })

  const errors = {
    name: nameError(form.name),
    gpuCount: intError(form.gpuCount, 1, 64),
    storage: buildStorage(form.storageAmount, form.storageUnit) ? null : 'Enter a whole number from 1 to 999999.',
    idle: buildIdleTimeout(form.idleAmount, form.idleUnit) ? null : 'Enter a whole number from 1 to 99999.',
  }
  const invalid = Object.values(errors).some(Boolean)
  const show = (e: string | null, always = touched) => (always ? e : null)
  const nameShown = show(errors.name, touched || form.name !== '')

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    setTouched(true)
    if (invalid) return
    createMutation.mutate({
      name: form.name,
      type: form.type,
      gpuCount: parseIntStrict(form.gpuCount) as number,
      gpuType: form.gpuType,
      storageSize: buildStorage(form.storageAmount, form.storageUnit) as string,
      idleTimeout: buildIdleTimeout(form.idleAmount, form.idleUnit) as string,
    })
  }

  const gpuErr = show(errors.gpuCount)
  const storageErr = show(errors.storage)
  const idleErr = show(errors.idle)
  const msg = (id: string, err: string | null | undefined, hint?: string) =>
    err || hint ? (
      <span id={`${uid}-${id}-msg`} className={err ? 'warning' : 'faint'}>
        {err ?? hint}
      </span>
    ) : null

  return (
    <Modal title="Create workspace" onClose={onClose} dirty={dirty}>
      <form onSubmit={handleSubmit} className="stack" noValidate>
        <label className="field">
          Name
          <input
            type="text"
            data-autofocus
            value={form.name}
            onChange={(e) => set({ name: e.target.value })}
            placeholder="my-workspace"
            autoComplete="off"
            spellCheck={false}
            {...fieldAria(`${uid}-name`, nameShown, true)}
          />
          {msg('name', nameShown, 'Lowercase letters, digits and hyphens.')}
        </label>

        <div className="formgrid">
          <label className="field">
            Type
            <select value={form.type} onChange={(e) => set({ type: e.target.value })}>
              <option value="jupyter">Jupyter</option>
              <option value="vscode">VS Code</option>
            </select>
          </label>
          <label className="field">
            GPU type
            <select value={form.gpuType} onChange={(e) => set({ gpuType: e.target.value })}>
              {['H100', 'A100-80G', 'A100-40G', 'L40', 'V100', 'T4'].map((g) => (
                <option key={g} value={g}>
                  {g}
                </option>
              ))}
            </select>
          </label>
        </div>

        <div className="formgrid">
          <label className="field">
            GPU count
            <input type="number" min={1} max={64} value={form.gpuCount} onChange={(e) => set({ gpuCount: e.target.value })} {...fieldAria(`${uid}-gpu`, gpuErr)} />
            {msg('gpu', gpuErr)}
          </label>
          <div className="field">
            <label htmlFor={`${uid}-storage`}>Storage</label>
            <div className="toolbar">
              <input id={`${uid}-storage`} type="number" min={1} max={999999} value={form.storageAmount} onChange={(e) => set({ storageAmount: e.target.value })} {...fieldAria(`${uid}-storage`, storageErr)} />
              <select aria-label="Storage unit" value={form.storageUnit} onChange={(e) => set({ storageUnit: e.target.value })}>
                <option value="Gi">Gi</option>
                <option value="Ti">Ti</option>
              </select>
            </div>
            {msg('storage', storageErr)}
          </div>
          <div className="field">
            <label htmlFor={`${uid}-idle`}>Idle timeout</label>
            <div className="toolbar">
              <input id={`${uid}-idle`} type="number" min={1} max={99999} value={form.idleAmount} onChange={(e) => set({ idleAmount: e.target.value })} {...fieldAria(`${uid}-idle`, idleErr, true)} />
              <select aria-label="Idle timeout unit" value={form.idleUnit} onChange={(e) => set({ idleUnit: e.target.value })}>
                <option value="m">minutes</option>
                <option value="h">hours</option>
              </select>
            </div>
            {msg('idle', idleErr, 'The workspace goes idle after this long without activity.')}
          </div>
        </div>

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
            {createMutation.isPending ? 'Creating…' : 'Create workspace'}
          </button>
        </div>
      </form>
    </Modal>
  )
}
