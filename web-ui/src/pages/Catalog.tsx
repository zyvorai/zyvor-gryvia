// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useId, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { errorMessage } from '@/lib/errors'
import { notify } from '@/lib/notify'
import { fieldAria } from '@/lib/forms'
import { useIsAdmin } from '@/lib/useRole'
import { EMPTY_SKU_FORM, buildSkuBody, formatRate, skuErrors, skuToForm, spotRate, visibleSkus, type Sku, type SkuForm } from '@/lib/cloud'
import { formatPercent } from '@/lib/format'
import type { FilterDef, SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import DataTable, { type Column } from '@/components/DataTable'
import PageHero from '@/components/PageHero'
import Modal from '@/components/Modal'
import ConfirmDialog from '@/components/ConfirmDialog'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

const FILTERS: FilterDef<Sku>[] = [{ name: 'gpu', label: 'GPU type', get: (s) => s.spec.gpuType }]
const SORTS: Record<string, SortAccessor<Sku>> = {
  name: (s) => s.metadata.name,
  gpu: (s) => s.spec.gpuType,
  gpus: (s) => s.spec.gpusPerUnit ?? 1,
  rate: (s) => s.spec.hourlyRate,
  spot: (s) => s.spec.spotDiscount ?? 0,
}
const searchText = (s: Sku) => `${s.metadata.name} ${s.spec.gpuType} ${s.spec.description ?? ''}`

export default function Catalog() {
  useDocumentTitle('Catalog')
  const admin = useIsAdmin()
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState<Sku | 'new' | null>(null)
  const [deleting, setDeleting] = useState<string | null>(null)

  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({ queryKey: ['skus'], queryFn: api.getSkus, refetchInterval: 60000 })
  const skus = data ? visibleSkus(data, admin) : undefined
  const table = useTableState({ rows: skus, searchText, filters: FILTERS, sortAccessors: SORTS, defaultSort: { key: 'name', dir: 'asc' }, pageSize: 12 })

  const deleteMutation = useMutation({
    mutationFn: (name: string) => api.deleteSku(name),
    onSuccess: (_d, name) => {
      notify.success(`Deleted SKU ${name}`)
      setDeleting(null)
      return queryClient.invalidateQueries({ queryKey: ['skus'] })
    },
    onError: (err, name) => {
      notify.error(`Could not delete ${name}`, err)
      setDeleting(null)
    },
  })

  const hero = <PageHero eyebrow="Catalog" title="GPU catalog." lede="The GPU types on offer and what an hour of each costs. Rates are what usage is estimated from; no payment is taken." />

  if (isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={4} />
      </>
    )
  }
  if (isError && !data) {
    return (
      <>
        <PageHero eyebrow="Catalog" title="Catalog unavailable." tint="red" />
        <ErrorState title="Could not load the catalog." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  const columns: Column<Sku>[] = [
    { key: 'name', header: 'SKU', sortable: true, render: (s) => <span className="mono">{s.metadata.name}</span> },
    { key: 'gpu', header: 'GPU type', sortable: true, render: (s) => s.spec.gpuType },
    { key: 'gpus', header: 'GPUs / unit', sortable: true, numeric: true, render: (s) => s.spec.gpusPerUnit ?? 1 },
    { key: 'rate', header: 'Rate / hour', sortable: true, numeric: true, render: (s) => formatRate(s.spec.hourlyRate, s.spec.currency) },
    {
      key: 'spot',
      header: 'Spot discount',
      sortable: true,
      numeric: true,
      render: (s) =>
        s.spec.spotDiscount ? (
          <span title={`Spot rate ${formatRate(spotRate(s.spec.hourlyRate, s.spec.spotDiscount), s.spec.currency)} / hour`}>{formatPercent(s.spec.spotDiscount)}</span>
        ) : (
          '—'
        ),
    },
    { key: 'description', header: 'Description', render: (s) => s.spec.description || <span className="faint">—</span> },
  ]
  if (admin) {
    columns.splice(1, 0, { key: 'enabled', header: 'Status', render: (s) => <span className={`pill ${s.spec.enabled === false ? 'warn' : 'ok'}`}>{s.spec.enabled === false ? 'Disabled' : 'Enabled'}</span> })
    columns.push({
      key: 'actions',
      header: 'Actions',
      render: (s) => (
        <div className="toolbar">
          <button type="button" className="btn-secondary" onClick={() => setEditing(s)} aria-label={`Edit SKU ${s.metadata.name}`}>
            Edit
          </button>
          <button type="button" className="danger" onClick={() => setDeleting(s.metadata.name)} aria-label={`Delete SKU ${s.metadata.name}`}>
            Delete
          </button>
        </div>
      ),
    })
  }

  return (
    <>
      {hero}
      <div className="grid">
        {isError && (
          <div className="span3">
            <ErrorState title="Could not refresh the catalog; showing the last data." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}
        <section className="card span3">
          <p className="eyebrow">SKUs</p>
          <h2 className="card-title">{admin ? 'All SKUs' : 'Available SKUs'}</h2>
          <DataTable
            caption="GPU SKUs and hourly rates"
            columns={columns}
            state={table}
            rowKey={(s) => s.metadata.name}
            searchLabel="Search SKUs"
            actions={
              admin && (
                <button type="button" className="primary" onClick={() => setEditing('new')}>
                  New SKU
                </button>
              )
            }
            empty={
              <EmptyState
                title="No SKUs yet."
                action={
                  admin && (
                    <button type="button" className="primary" onClick={() => setEditing('new')}>
                      Create the first SKU
                    </button>
                  )
                }
              >
                {admin ? 'A SKU is a GPU type with an hourly rate. Tenants can only use SKUs you allow them.' : 'Your provider has not published any GPU SKUs yet.'}
              </EmptyState>
            }
          />
        </section>
      </div>

      {deleting && (
        <ConfirmDialog title={`Delete SKU ${deleting}?`} confirmLabel="Delete SKU" busy={deleteMutation.isPending} onCancel={() => setDeleting(null)} onConfirm={() => deleteMutation.mutate(deleting)}>
          Tenants that list {deleting} as allowed lose access to it. Usage already recorded is kept. This cannot be undone.
        </ConfirmDialog>
      )}
      {editing && <SkuModal sku={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
    </>
  )
}

function SkuModal({ sku, onClose }: { sku: Sku | null; onClose: () => void }) {
  const queryClient = useQueryClient()
  const uid = useId()
  const initial = sku ? skuToForm(sku) : EMPTY_SKU_FORM
  const [form, setForm] = useState<SkuForm>(initial)
  const [touched, setTouched] = useState(false)
  const set = (patch: Partial<SkuForm>) => setForm((f) => ({ ...f, ...patch }))
  const dirty = JSON.stringify(form) !== JSON.stringify(initial)
  const errors = skuErrors(form, !!sku)
  const shown = (k: keyof typeof errors, always = touched) => (always ? errors[k] : undefined)

  const save = useMutation({
    mutationFn: (f: SkuForm) => {
      const { name, ...body } = buildSkuBody(f)
      return sku ? api.updateSku(sku.metadata.name, body) : api.createSku({ name, ...body })
    },
    onSuccess: (_d, f) => {
      notify.success(sku ? `Updated SKU ${sku.metadata.name}` : `Created SKU ${f.name.trim()}`)
      queryClient.invalidateQueries({ queryKey: ['skus'] })
      onClose()
    },
    onError: (err) => notify.error(sku ? 'Could not update SKU' : 'Could not create SKU', err),
  })

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setTouched(true)
    if (Object.keys(errors).length) return
    save.mutate(form)
  }
  const msg = (id: string, err?: string, hint?: string) =>
    err || hint ? (
      <span id={`${uid}-${id}-msg`} className={err ? 'warning' : 'faint'}>
        {err ?? hint}
      </span>
    ) : null

  const nameErr = shown('name', touched || form.name !== '')
  return (
    <Modal title={sku ? `Edit SKU ${sku.metadata.name}` : 'Create SKU'} onClose={onClose} dirty={dirty}>
      <form onSubmit={submit} className="stack" noValidate>
        {!sku && (
          <label className="field">
            Name
            <input type="text" data-autofocus value={form.name} onChange={(e) => set({ name: e.target.value })} placeholder="a100-80g" autoComplete="off" spellCheck={false} {...fieldAria(`${uid}-name`, nameErr, true)} />
            {msg('name', nameErr, 'Lowercase letters, digits and hyphens. It cannot be changed later.')}
          </label>
        )}
        <div className="formgrid">
          <label className="field">
            GPU type
            <input type="text" data-autofocus={sku ? '' : undefined} value={form.gpuType} onChange={(e) => set({ gpuType: e.target.value })} placeholder="A100-80G" autoComplete="off" {...fieldAria(`${uid}-gpuType`, shown('gpuType'))} />
            {msg('gpuType', shown('gpuType'))}
          </label>
          <label className="field">
            GPUs per unit
            <input type="number" min={1} max={64} value={form.gpusPerUnit} onChange={(e) => set({ gpusPerUnit: e.target.value })} {...fieldAria(`${uid}-gpusPerUnit`, shown('gpusPerUnit'))} />
            {msg('gpusPerUnit', shown('gpusPerUnit'))}
          </label>
        </div>
        <div className="formgrid">
          <label className="field">
            Rate per hour
            <input type="text" inputMode="decimal" value={form.hourlyRate} onChange={(e) => set({ hourlyRate: e.target.value })} placeholder="2.50" {...fieldAria(`${uid}-hourlyRate`, shown('hourlyRate'), true)} />
            {msg('hourlyRate', shown('hourlyRate'), 'For one unit, per hour.')}
          </label>
          <label className="field">
            Currency
            <input type="text" value={form.currency} onChange={(e) => set({ currency: e.target.value })} maxLength={3} autoComplete="off" {...fieldAria(`${uid}-currency`, shown('currency'))} />
            {msg('currency', shown('currency'))}
          </label>
          <label className="field">
            Spot discount (%)
            <input type="number" min={0} max={100} value={form.spotDiscount} onChange={(e) => set({ spotDiscount: e.target.value })} {...fieldAria(`${uid}-spotDiscount`, shown('spotDiscount'))} />
            {msg('spotDiscount', shown('spotDiscount'))}
          </label>
        </div>
        <label className="field">
          Description
          <input type="text" value={form.description} onChange={(e) => set({ description: e.target.value })} placeholder="80 GB, NVLink" autoComplete="off" />
        </label>
        <label className="field">
          <span>
            <input type="checkbox" checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} /> Enabled (visible to tenants)
          </span>
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
            {save.isPending ? 'Saving…' : sku ? 'Save changes' : 'Create SKU'}
          </button>
        </div>
      </form>
    </Modal>
  )
}
