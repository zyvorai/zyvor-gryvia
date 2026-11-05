import { useId, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '@/lib/api'
import { errorMessage } from '@/lib/errors'
import { notify } from '@/lib/notify'
import { fieldAria } from '@/lib/forms'
import { useIsAdmin } from '@/lib/useRole'
import { EMPTY_TENANT_FORM, buildTenantBody, tenantErrors, toggleValue, type TenantForm, type TenantResource } from '@/lib/cloud'
import type { SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import DataTable, { type Column } from '@/components/DataTable'
import PageHero from '@/components/PageHero'
import Modal from '@/components/Modal'
import ConfirmDialog from '@/components/ConfirmDialog'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

const nameOf = (t: TenantResource) => t.metadata.name
const SORTS: Record<string, SortAccessor<TenantResource>> = {
  name: nameOf,
  health: (t) => t.status?.health,
  members: (t) => t.status?.memberCount ?? 0,
  gpus: (t) => t.spec?.quotas?.maxGPUs ?? 0,
}
const searchText = (t: TenantResource) => `${nameOf(t)} ${t.spec?.displayName ?? ''} ${(t.spec?.allowedSkus ?? []).join(' ')}`

const healthTone = (h?: string) => (h === 'Healthy' ? 'ok' : h ? 'warn' : '')

export default function Tenants() {
  useDocumentTitle('Tenants')
  const admin = useIsAdmin()
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [deleting, setDeleting] = useState<string | null>(null)

  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({ queryKey: ['tenants'], queryFn: api.getTenants, refetchInterval: 30000 })
  const fairshare = useQuery({ queryKey: ['fairshare', 7], queryFn: () => api.getFairshare(7), enabled: admin, refetchInterval: 60000 })
  const fairshareApply = useMutation({
    mutationFn: (dryRun: boolean) => api.applyFairshare({ dryRun }, 7),
    onSuccess: (res, dryRun) => {
      const n = res.applied?.length ?? 0
      notify.success(dryRun ? `Fairshare dry-run: ${n} change(s)` : `Applied fairshare to ${n} tenant(s)`)
      if (!dryRun) queryClient.invalidateQueries({ queryKey: ['fairshare'] })
      queryClient.invalidateQueries({ queryKey: ['tenants'] })
    },
    onError: (err) => notify.error('Fairshare apply failed', err),
  })
  const table = useTableState({ rows: data, searchText, sortAccessors: SORTS, defaultSort: { key: 'name', dir: 'asc' }, pageSize: 12 })

  const deleteMutation = useMutation({
    mutationFn: (name: string) => api.deleteTenant(name),
    onSuccess: (_d, name) => {
      notify.success(`Deleted tenant ${name}`)
      setDeleting(null)
      return queryClient.invalidateQueries({ queryKey: ['tenants'] })
    },
    onError: (err, name) => {
      notify.error(`Could not delete ${name}`, err)
      setDeleting(null)
    },
  })

  const hero = admin ? (
    <PageHero eyebrow="Tenants" title="Tenants." lede="Each tenant gets its own namespace, a GPU limit and a list of SKUs it may use." />
  ) : (
    <PageHero eyebrow="Tenant" title="Your tenant." lede="What your provider has set up for your account. Only an admin can change it." />
  )

  if (isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={3} />
      </>
    )
  }
  if (isError && !data) {
    return (
      <>
        <PageHero eyebrow="Tenants" title="Tenants unavailable." tint="red" />
        <ErrorState title="Could not load tenants." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  if (!admin) {
    const mine = data ?? []
    return (
      <>
        {hero}
        <div className="grid">
          {mine.length === 0 ? (
            <section className="card span3">
              <EmptyState title="No tenant found.">Your sign-in is not linked to a tenant yet. Ask your provider to add you.</EmptyState>
            </section>
          ) : (
            mine.map((t) => (
              <section key={nameOf(t)} className="card span3" aria-labelledby={`tenant-${nameOf(t)}-title`}>
                <p className="eyebrow">Tenant</p>
                <h2 className="card-title" id={`tenant-${nameOf(t)}-title`}>
                  {t.spec?.displayName || nameOf(t)}
                </h2>
                <p>
                  <span className="mono faint">{nameOf(t)}</span> {t.status?.health && <span className={`pill ${healthTone(t.status.health)}`}>{t.status.health}</span>}
                </p>
                <div className="apple-metric-band">
                  <div>
                    <span>Namespace</span>
                    <b className="mono">{t.spec?.namespace ?? '—'}</b>
                  </div>
                  <div>
                    <span>GPU limit</span>
                    <b>{t.spec?.quotas?.maxGPUs ?? '—'}</b>
                  </div>
                  <div>
                    <span>Members</span>
                    <b>{t.status?.memberCount ?? '—'}</b>
                  </div>
                </div>
                <p className="eyebrow">Allowed SKUs</p>
                <div className="row">
                  {(t.spec?.allowedSkus ?? []).length === 0 ? (
                    <span className="faint">None listed</span>
                  ) : (
                    t.spec?.allowedSkus?.map((s) => (
                      <span key={s} className="pill mono">
                        {s}
                      </span>
                    ))
                  )}
                </div>
                <p>
                  <Link to="/catalog" className="card-link">
                    See rates in the catalog ›
                  </Link>
                </p>
              </section>
            ))
          )}
        </div>
      </>
    )
  }

  const columns: Column<TenantResource>[] = [
    {
      key: 'name',
      header: 'Tenant',
      sortable: true,
      render: (t) => (
        <>
          <span className="mono">{nameOf(t)}</span>
          {t.spec?.displayName && <span className="faint"> · {t.spec.displayName}</span>}
        </>
      ),
    },
    { key: 'health', header: 'Health', sortable: true, render: (t) => (t.status?.health ? <span className={`pill ${healthTone(t.status.health)}`}>{t.status.health}</span> : <span className="faint">—</span>) },
    { key: 'namespace', header: 'Namespace', render: (t) => <span className="mono">{t.spec?.namespace ?? '—'}</span> },
    { key: 'gpus', header: 'GPU limit', sortable: true, numeric: true, render: (t) => t.spec?.quotas?.maxGPUs ?? '—' },
    { key: 'members', header: 'Members', sortable: true, numeric: true, render: (t) => t.status?.memberCount ?? '—' },
    { key: 'skus', header: 'Allowed SKUs', render: (t) => ((t.spec?.allowedSkus ?? []).length ? t.spec?.allowedSkus?.join(', ') : <span className="faint">None</span>) },
    {
      key: 'actions',
      header: 'Actions',
      render: (t) => (
        <button type="button" className="danger" onClick={() => setDeleting(nameOf(t))} aria-label={`Delete tenant ${nameOf(t)}`}>
          Delete
        </button>
      ),
    },
  ]

  return (
    <>
      {hero}
      <div className="grid">
        {isError && (
          <div className="span3">
            <ErrorState title="Could not refresh tenants; showing the last data." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}
        {fairshare.data && (
          <section className="card span3" aria-labelledby="fairshare-title">
            <p className="eyebrow">Fairshare</p>
            <h2 className="card-title" id="fairshare-title">
              Lookback weights
            </h2>
            <p className="faint">
              Last {fairshare.data.lookbackDays} days · {Math.round(fairshare.data.totalGpuHours * 100) / 100} GPU-hours
              {fairshare.data.applyEnabled ? '' : ' · recommend-only'}
            </p>
            <div className="apple-metric-band">
              {(fairshare.data.items ?? []).slice(0, 6).map((r) => (
                <div key={r.tenant}>
                  <span className="mono">{r.tenant}</span>
                  <b>
                    {r.weight} → {r.recommendedWeight}
                  </b>
                </div>
              ))}
              {(fairshare.data.items ?? []).length === 0 && <div><span>No tenants</span><b>—</b></div>}
            </div>
            <div className="row" style={{ gap: '0.5rem', marginTop: '0.75rem', flexWrap: 'wrap' }}>
              <button type="button" className="btn-secondary" disabled={fairshareApply.isPending}
                onClick={() => fairshareApply.mutate(true)}>Preview apply</button>
              <button type="button" className="primary" disabled={fairshareApply.isPending || !fairshare.data.applyEnabled}
                title={fairshare.data.applyEnabled ? 'Patch fairShareWeight on tenants' : 'Enable apiGateway.fairshareApply (or use CLI gryvia tenant fairshare --apply)'}
                onClick={() => fairshareApply.mutate(false)}>Apply weights</button>
            </div>
            <p className="faint">CLI: <span className="mono">gryvia tenant fairshare --apply</span></p>
          </section>
        )}
        <section className="card span3">
          <p className="eyebrow">Accounts</p>
          <h2 className="card-title">All tenants</h2>
          <DataTable
            caption="Tenants"
            columns={columns}
            state={table}
            rowKey={nameOf}
            searchLabel="Search tenants"
            actions={
              <button type="button" className="primary" onClick={() => setCreating(true)}>
                New tenant
              </button>
            }
            empty={
              <EmptyState
                title="No tenants yet."
                action={
                  <button type="button" className="primary" onClick={() => setCreating(true)}>
                    Create the first tenant
                  </button>
                }
              >
                A tenant is a customer or team with its own namespace, GPU limit and SKU list.
              </EmptyState>
            }
          />
        </section>
      </div>

      {deleting && (
        <ConfirmDialog title={`Delete tenant ${deleting}?`} confirmLabel="Delete tenant" busy={deleteMutation.isPending} onCancel={() => setDeleting(null)} onConfirm={() => deleteMutation.mutate(deleting)}>
          Deleting {deleting} removes its access and can remove its managed namespace and the workloads in it. This cannot be undone.
        </ConfirmDialog>
      )}
      {creating && <CreateTenantModal onClose={() => setCreating(false)} />}
    </>
  )
}

function CreateTenantModal({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient()
  const uid = useId()
  const [form, setForm] = useState<TenantForm>(EMPTY_TENANT_FORM)
  const [touched, setTouched] = useState(false)
  const set = (patch: Partial<TenantForm>) => setForm((f) => ({ ...f, ...patch }))
  const dirty = JSON.stringify(form) !== JSON.stringify(EMPTY_TENANT_FORM)
  const errors = tenantErrors(form)
  const skus = useQuery({ queryKey: ['skus'], queryFn: api.getSkus })

  const create = useMutation({
    mutationFn: (f: TenantForm) => api.createTenant(buildTenantBody(f)),
    onSuccess: (_d, f) => {
      notify.success(`Created tenant ${f.name.trim()}`)
      queryClient.invalidateQueries({ queryKey: ['tenants'] })
      onClose()
    },
    onError: (err) => notify.error('Could not create tenant', err),
  })

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setTouched(true)
    if (Object.keys(errors).length) return
    create.mutate(form)
  }
  const nameErr = touched || form.name !== '' ? errors.name : undefined
  const maxErr = touched ? errors.maxGPUs : undefined

  return (
    <Modal title="Create tenant" onClose={onClose} dirty={dirty}>
      <form onSubmit={submit} className="stack" noValidate>
        <label className="field">
          Name
          <input type="text" data-autofocus value={form.name} onChange={(e) => set({ name: e.target.value })} placeholder="acme" autoComplete="off" spellCheck={false} {...fieldAria(`${uid}-name`, nameErr, true)} />
          <span id={`${uid}-name-msg`} className={nameErr ? 'warning' : 'faint'}>
            {nameErr ?? 'Lowercase letters, digits and hyphens. The namespace will be tenant-<name>.'}
          </span>
        </label>
        <label className="field">
          Display name
          <input type="text" value={form.displayName} onChange={(e) => set({ displayName: e.target.value })} placeholder="Acme Corp" autoComplete="off" />
        </label>
        <fieldset className="field">
          <legend>Allowed SKUs</legend>
          {skus.isLoading ? (
            <span className="faint" role="status">Loading SKUs…</span>
          ) : (skus.data ?? []).length === 0 ? (
            <span className="faint">No SKUs in the catalog yet. Create some first, or add them to the tenant later.</span>
          ) : (
            <div className="stack">
              {(skus.data ?? []).map((s) => (
                <label key={s.metadata.name}>
                  <input type="checkbox" checked={form.allowedSkus.includes(s.metadata.name)} onChange={() => set({ allowedSkus: toggleValue(form.allowedSkus, s.metadata.name) })} /> <span className="mono">{s.metadata.name}</span> <span className="faint">{s.spec.gpuType}</span>
                </label>
              ))}
            </div>
          )}
        </fieldset>
        <label className="field">
          Max GPUs
          <input type="number" min={0} value={form.maxGPUs} onChange={(e) => set({ maxGPUs: e.target.value })} placeholder="Leave blank for the default" {...fieldAria(`${uid}-max`, maxErr)} />
          {maxErr && (
            <span id={`${uid}-max-msg`} className="warning">
              {maxErr}
            </span>
          )}
        </label>
        <label className="field">
          <span>
            <input type="checkbox" checked={form.isolated} onChange={(e) => set({ isolated: e.target.checked })} /> Isolate network traffic from other tenants
          </span>
        </label>
        {create.isError && (
          <p className="warning" role="alert">
            {errorMessage(create.error)}
          </p>
        )}
        <div className="toolbar">
          <button type="button" className="btn-secondary" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={create.isPending}>
            {create.isPending ? 'Creating…' : 'Create tenant'}
          </button>
        </div>
      </form>
    </Modal>
  )
}
