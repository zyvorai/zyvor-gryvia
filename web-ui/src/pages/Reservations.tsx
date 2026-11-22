// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useId, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { errorMessage } from '@/lib/errors'
import { notify } from '@/lib/notify'
import { fieldAria } from '@/lib/forms'
import { EMPTY_RESERVATION_FORM, buildReservationBody, canCancel, ownerLabel, reservationErrors, stateTone, type Reservation, type ReservationForm } from '@/lib/reservations'
import { formatDate } from '@/lib/format'
import type { FilterDef, SortAccessor } from '@/lib/tableState'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { useTableState } from '@/hooks/useTableState'
import DataTable, { type Column } from '@/components/DataTable'
import PageHero from '@/components/PageHero'
import Modal from '@/components/Modal'
import ConfirmDialog from '@/components/ConfirmDialog'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

const FILTERS: FilterDef<Reservation>[] = [{ name: 'state', label: 'State', get: (r) => r.state ?? '' }]
const SORTS: Record<string, SortAccessor<Reservation>> = {
  name: (r) => r.name,
  owner: (r) => ownerLabel(r),
  gpus: (r) => r.gpuCount ?? 0,
  state: (r) => r.state ?? '',
  end: (r) => r.schedule?.endTime ?? '',
}
const searchText = (r: Reservation) => `${r.name} ${ownerLabel(r)} ${r.gpuType ?? ''} ${(r.allocatedNodes ?? []).join(' ')}`

export default function Reservations() {
  useDocumentTitle('Reservations')
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [cancelling, setCancelling] = useState<string | null>(null)

  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({ queryKey: ['reservations'], queryFn: api.getReservations, refetchInterval: 30000 })
  const table = useTableState({ rows: data, searchText, filters: FILTERS, sortAccessors: SORTS, defaultSort: { key: 'name', dir: 'asc' }, pageSize: 12 })

  const cancel = useMutation({
    mutationFn: (name: string) => api.cancelReservation(name),
    onSuccess: (_d, name) => {
      notify.success(`Cancelled reservation ${name}`)
      setCancelling(null)
      return queryClient.invalidateQueries({ queryKey: ['reservations'] })
    },
    onError: (err, name) => {
      notify.error(`Could not cancel ${name}`, err)
      setCancelling(null)
    },
  })

  const hero = (
    <PageHero
      eyebrow="Reservations"
      title="Reserved GPU nodes."
      lede="A reservation taints whole nodes so only its owner's jobs (annotated gryvia.io/reservation) can land there. Everyone else's jobs cannot schedule on them."
    />
  )
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
        <PageHero eyebrow="Reservations" title="Reservations unavailable." tint="red" />
        <ErrorState title="Could not load reservations." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  const columns: Column<Reservation>[] = [
    { key: 'name', header: 'Name', sortable: true, render: (r) => <span className="mono">{r.name}</span> },
    { key: 'owner', header: 'Owner', sortable: true, render: (r) => ownerLabel(r) },
    { key: 'gpus', header: 'GPUs', sortable: true, numeric: true, render: (r) => `${r.allocatedGPUs ?? 0} / ${r.gpuCount ?? '—'} ${r.gpuType ?? ''}` },
    { key: 'nodes', header: 'Nodes', render: (r) => (r.allocatedNodes?.length ? <span className="mono">{r.allocatedNodes.join(', ')}</span> : <span className="faint">—</span>) },
    { key: 'state', header: 'State', sortable: true, render: (r) => <span className={`pill ${stateTone(r.state)}`}>{r.state ?? 'unknown'}</span> },
    { key: 'end', header: 'Ends', sortable: true, render: (r) => (r.schedule?.endTime ? formatDate(r.schedule.endTime) : <span className="faint">—</span>) },
    {
      key: 'actions',
      header: 'Actions',
      render: (r) =>
        canCancel(r) ? (
          <button type="button" className="danger" onClick={() => setCancelling(r.name)} aria-label={`Cancel reservation ${r.name}`}>
            Cancel
          </button>
        ) : null,
    },
  ]

  return (
    <>
      {hero}
      <div className="grid">
        {isError && (
          <div className="span3">
            <ErrorState title="Could not refresh reservations; showing the last data." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}
        <section className="card span3">
          <p className="eyebrow">Reservations</p>
          <h2 className="card-title">All reservations</h2>
          <DataTable
            caption="GPU node reservations"
            columns={columns}
            state={table}
            rowKey={(r) => r.name}
            searchLabel="Search reservations"
            actions={
              <button type="button" className="primary" onClick={() => setCreating(true)}>
                New reservation
              </button>
            }
            empty={<EmptyState title="No reservations.">Reserve nodes when a team needs guaranteed capacity.</EmptyState>}
          />
        </section>
      </div>
      {cancelling && (
        <ConfirmDialog title={`Cancel reservation ${cancelling}?`} confirmLabel="Cancel reservation" busy={cancel.isPending} onCancel={() => setCancelling(null)} onConfirm={() => cancel.mutate(cancelling)}>
          The nodes are released (taint and labels removed). Running jobs are not stopped.
        </ConfirmDialog>
      )}
      {creating && <ReservationModal onClose={() => setCreating(false)} />}
    </>
  )
}

function ReservationModal({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient()
  const uid = useId()
  const [form, setForm] = useState<ReservationForm>(EMPTY_RESERVATION_FORM)
  const [touched, setTouched] = useState(false)
  const set = (patch: Partial<ReservationForm>) => setForm((f) => ({ ...f, ...patch }))
  const errors = reservationErrors(form)
  const shown = (k: keyof typeof errors) => (touched ? errors[k] : undefined)
  const dirty = JSON.stringify(form) !== JSON.stringify(EMPTY_RESERVATION_FORM)

  const save = useMutation({
    mutationFn: (f: ReservationForm) => api.createReservation(buildReservationBody(f)),
    onSuccess: () => {
      notify.success('Reservation created')
      queryClient.invalidateQueries({ queryKey: ['reservations'] })
      onClose()
    },
    onError: (err) => notify.error('Could not create reservation', err),
  })
  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setTouched(true)
    if (Object.keys(errors).length) return
    save.mutate(form)
  }
  const msg = (id: string, err?: string) => (err ? <span id={`${uid}-${id}-msg`} className="warning">{err}</span> : null)

  return (
    <Modal title="New reservation" onClose={onClose} dirty={dirty}>
      <form onSubmit={submit} className="stack" noValidate>
        <div className="formgrid">
          <label className="field">
            GPU type
            <input type="text" data-autofocus value={form.gpuType} onChange={(e) => set({ gpuType: e.target.value })} placeholder="A100-80G" autoComplete="off" {...fieldAria(`${uid}-gpuType`, shown('gpuType'))} />
            {msg('gpuType', shown('gpuType'))}
          </label>
          <label className="field">
            GPUs
            <input type="number" min={1} value={form.gpuCount} onChange={(e) => set({ gpuCount: e.target.value })} {...fieldAria(`${uid}-gpuCount`, shown('gpuCount'))} />
            {msg('gpuCount', shown('gpuCount'))}
          </label>
        </div>
        <div className="formgrid">
          <label className="field">
            Start (blank = now)
            <input type="datetime-local" value={form.startTime} onChange={(e) => set({ startTime: e.target.value })} {...fieldAria(`${uid}-startTime`, shown('startTime'))} />
            {msg('startTime', shown('startTime'))}
          </label>
          <label className="field">
            End
            <input type="datetime-local" value={form.endTime} onChange={(e) => set({ endTime: e.target.value })} {...fieldAria(`${uid}-endTime`, shown('endTime'))} />
            {msg('endTime', shown('endTime'))}
          </label>
        </div>
        <label className="field">
          Name (optional)
          <input type="text" value={form.name} onChange={(e) => set({ name: e.target.value })} autoComplete="off" spellCheck={false} {...fieldAria(`${uid}-name`, shown('name'))} />
          {msg('name', shown('name'))}
        </label>
        <label className="field">
          <span>
            <input type="checkbox" checked={form.exclusive} onChange={(e) => set({ exclusive: e.target.checked })} /> Exclusive to the owner
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
            {save.isPending ? 'Creating…' : 'Create reservation'}
          </button>
        </div>
      </form>
    </Modal>
  )
}
