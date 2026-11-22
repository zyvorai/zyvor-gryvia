// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useId, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router-dom'
import { api } from '@/lib/api'
import type { FlowPolicy, CreateFlowPolicyRequest } from '@/lib/api'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import ConfirmDialog from '@/components/ConfirmDialog'
import DataTable, { type Column } from '@/components/DataTable'
import Modal from '@/components/Modal'
import { useTableState } from '@/hooks/useTableState'
import type { FilterDef, SortAccessor } from '@/lib/tableState'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { notify } from '@/lib/notify'
import { errorMessage } from '@/lib/errors'
import { formatPercent } from '@/lib/format'
import { phaseTone } from '@/lib/phase'
import { bucketPolicies, flowsUrlForService, INTENT_MAX, parsePolicyPrefill, POLICY_PROTOCOLS, validatePolicyForm, type PolicyFormErrors, type PolicyFormValues, type PolicyPrefill } from '@/lib/network'

type PolicyAction = 'allow' | 'deny' | 'log'
type PolicyStatusExt = { phase?: string; matchedFlows?: number; ciliumPolicyRef?: string }

const phaseOf = (p: FlowPolicy) => p.status?.phase
const phaseLabel = (p: FlowPolicy) => p.status?.phase || 'Pending'

const FILTERS: FilterDef<FlowPolicy>[] = [
  { name: 'action', label: 'Action', get: (p) => p.spec.action, options: ['allow', 'deny', 'log'] },
  { name: 'phase', label: 'Phase', get: phaseLabel },
]
const SORTERS: Record<string, SortAccessor<FlowPolicy>> = {
  name: (p) => p.metadata.name,
  route: (p) => `${p.spec.sourceService} ${p.spec.destinationService}`,
  action: (p) => p.spec.action,
  phase: phaseLabel,
  matched: (p) => (p.status as PolicyStatusExt | undefined)?.matchedFlows,
}
const searchText = (p: FlowPolicy) => `${p.metadata.name} ${p.spec.sourceService} ${p.spec.destinationService} ${p.spec.protocol} ${p.spec.port} ${p.spec.action} ${phaseLabel(p)} ${p.spec.intent ?? ''}`

export default function NetworkPolicies() {
  useDocumentTitle('Network policies')
  const [searchParams, setSearchParams] = useSearchParams()
  // The create form can be opened by ?new=1&src&dst&port&protocol (from the flows page); read it once.
  const [prefill] = useState<PolicyPrefill>(() => parsePolicyPrefill(searchParams))
  const [showCreateForm, setShowCreateForm] = useState(prefill.open)
  const [confirming, setConfirming] = useState<FlowPolicy | null>(null)
  const [viewing, setViewing] = useState<FlowPolicy | null>(null)
  const closeCreate = () => {
    setShowCreateForm(false)
    if (searchParams.has('new')) {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          for (const k of ['new', 'src', 'dst', 'port', 'protocol']) next.delete(k)
          return next
        },
        { replace: true },
      )
    }
  }
  const queryClient = useQueryClient()

  const { data: policies, isLoading, isError, error, refetch, isFetching, dataUpdatedAt } = useQuery({
    queryKey: ['flowPolicies'],
    queryFn: api.getFlowPolicies,
    refetchInterval: 15000,
  })

  const applyMutation = useMutation({
    mutationFn: api.applyFlowPolicy,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['flowPolicies'] })
      notify.success('Apply requested — waiting for operator')
    },
    onError: (err) => notify.error('Could not request apply', err),
    onSettled: () => setConfirming(null),
  })

  const createMutation = useMutation({
    mutationFn: (req: CreateFlowPolicyRequest) => api.createFlowPolicy(req),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['flowPolicies'] })
      closeCreate()
      notify.success('Policy created')
    },
    onError: (err) => notify.error('Could not create policy', err),
  })

  const table = useTableState<FlowPolicy>({ rows: policies, searchText, filters: FILTERS, sortAccessors: SORTERS, pageSize: 15 })
  const applyingName = applyMutation.isPending ? (applyMutation.variables as string) : undefined
  const buckets = bucketPolicies(policies, phaseOf)
  const columns: Column<FlowPolicy>[] = [
    { key: 'name', header: 'Policy', sortable: true, render: (p) => <b>{p.metadata.name}</b> },
    {
      key: 'route',
      header: 'Connection',
      sortable: true,
      render: (p) => (
        <>
          {p.spec.sourceService} → {p.spec.destinationService} <span className="faint mono">{p.spec.protocol}:{p.spec.port}</span>
        </>
      ),
    },
    { key: 'action', header: 'Action', sortable: true, render: (p) => <ActionPill action={p.spec.action} /> },
    { key: 'phase', header: 'Phase', sortable: true, render: (p) => <PhaseBadge phase={phaseLabel(p)} /> },
    { key: 'matched', header: 'Matched flows', sortable: true, numeric: true, render: (p) => (p.status as PolicyStatusExt | undefined)?.matchedFlows ?? '—' },
    {
      key: 'actions',
      header: 'Actions',
      render: (p) => (
        <div className="toolbar">
          <Link to={flowsUrlForService(p.spec.sourceService)} className="buttonlike btn-secondary" onClick={(e) => e.stopPropagation()} aria-label={`View matching connections for ${p.metadata.name}`}>
            View matching connections
          </Link>
          {p.status?.phase?.toLowerCase() === 'suggested' && (
            <button
              type="button"
              className="btn-secondary"
              onClick={(e) => {
                e.stopPropagation()
                setConfirming(p)
              }}
              disabled={applyingName === p.metadata.name}
              aria-label={`Apply policy ${p.metadata.name}`}
            >
              {applyingName === p.metadata.name ? 'Requesting…' : 'Apply'}
            </button>
          )}
        </div>
      ),
    },
  ]
  const total = policies?.length

  return (
    <>
      <PageHero eyebrow="Network" title="Flow policies." lede="Review, create and apply the flow policies the network operator enforces." />

      <div className="grid">
        <div className="toolbar span3">
          {!showCreateForm && (
            <button type="button" className="primary" onClick={() => setShowCreateForm(true)}>
              New policy
            </button>
          )}
          <Link to="/network" className="buttonlike btn-secondary">
            Back to network
          </Link>
          <button type="button" className="btn-secondary" onClick={() => refetch()} disabled={isFetching} aria-busy={isFetching}>
            {isFetching ? 'Refreshing…' : 'Refresh'}
          </button>
        </div>

        {isLoading ? (
          <section className="card span3" aria-label="Live summary">
            <Skeleton rows={2} />
          </section>
        ) : isError && !policies ? (
          <div className="span3">
            <ErrorState title="Could not load flow policies." error={error} onRetry={() => refetch()} retrying={isFetching} />
          </div>
        ) : (
          <PagePulse
            updatedAt={dataUpdatedAt}
            error={isError ? errorMessage(error) : undefined}
            headline={
              buckets.suggested.length > 0
                ? `${buckets.suggested.length} suggested polic${buckets.suggested.length === 1 ? 'y' : 'ies'} awaiting review.`
                : `${buckets.enforced.length} polic${buckets.enforced.length === 1 ? 'y' : 'ies'} enforced.`
            }
            tone={buckets.suggested.length > 0 || buckets.pending.length > 0 ? 'warn' : undefined}
            figures={[
              { label: 'enforced', value: buckets.enforced.length },
              { label: 'pending', value: buckets.pending.length, tone: buckets.pending.length > 0 ? 'warn' : undefined },
              { label: 'suggested', value: buckets.suggested.length },
              ...(buckets.other.length > 0 ? [{ label: 'other', value: buckets.other.length }] : []),
              { label: 'total', value: total },
            ]}
          />
        )}

        {showCreateForm && (
          <CreatePolicyForm
            onSubmit={(data) => createMutation.mutate(data)}
            onCancel={closeCreate}
            initial={prefill}
            isSubmitting={createMutation.isPending}
            serverError={createMutation.isError ? errorMessage(createMutation.error) : undefined}
          />
        )}

        {policies && (
          <section className="card span3">
            <p className="eyebrow">{policies.length} TOTAL</p>
            <h2 className="card-title">Flow policies</h2>
            <DataTable
              caption="Flow policies"
              searchLabel="Search policies"
              columns={columns}
              state={table}
              rowKey={(p) => p.metadata.name}
              onRowClick={setViewing}
              empty={
                <EmptyState
                  title="No policies yet"
                  action={
                    !showCreateForm && (
                      <button type="button" className="primary" onClick={() => setShowCreateForm(true)}>
                        New policy
                      </button>
                    )
                  }
                >
                  Policies are GryviaFlowPolicy resources that the network operator enforces. Create one, or wait for the network-intelligence operator to suggest policies from observed traffic.
                </EmptyState>
              }
            />
          </section>
        )}
      </div>

      {confirming && (
        <ConfirmDialog
          title={`Apply policy ${confirming.metadata.name}?`}
          tone="primary"
          confirmLabel="Request apply"
          busy={applyMutation.isPending}
          onCancel={() => setConfirming(null)}
          onConfirm={() => applyMutation.mutate(confirming.metadata.name)}
        >
          This will {(confirming.spec.action as string) === 'log' ? 'log (observe only)' : confirming.spec.action} {confirming.spec.protocol}:{confirming.spec.port} traffic from {confirming.spec.sourceService} to {confirming.spec.destinationService}. Enforcement is requested from the network operator; the policy stays pending until the operator confirms it.
        </ConfirmDialog>
      )}

      {viewing && <PolicyDetailModal policy={viewing} onClose={() => setViewing(null)} />}
    </>
  )
}

function PolicyDetailModal({ policy, onClose }: { policy: FlowPolicy; onClose: () => void }) {
  const status = policy.status as PolicyStatusExt | undefined
  const confidence = typeof policy.spec.confidence === 'number' && policy.spec.confidence > 0 ? formatPercent(Math.round(policy.spec.confidence * 100)) : undefined
  const rows: Array<[string, string]> = [
    ['Connection', `${policy.spec.sourceService} → ${policy.spec.destinationService} · ${policy.spec.protocol}:${policy.spec.port}`],
    ['Action', policy.spec.action],
    ['Phase', phaseLabel(policy)],
    ['Intent', policy.spec.intent || '—'],
    ['Matched flows', status?.matchedFlows !== undefined ? String(status.matchedFlows) : 'not reported yet'],
    ['Cilium policy', status?.ciliumPolicyRef || 'not created yet'],
    ...(confidence ? ([['Confidence', confidence]] as Array<[string, string]>) : []),
  ]
  return (
    <Modal title={`Policy ${policy.metadata.name}`} onClose={onClose}>
      <div className="stack">
        {rows.map(([k, v]) => (
          <div key={k} className="list-row">
            <span className="grow faint">{k}</span>
            <span className="mono">{v}</span>
          </div>
        ))}
        <div className="toolbar">
          <Link to={flowsUrlForService(policy.spec.sourceService)} className="buttonlike btn-secondary">
            View matching connections
          </Link>
          <button type="button" className="btn-secondary" data-autofocus="" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </Modal>
  )
}

function ActionPill({ action }: { action: string }) {
  const tone = action === 'deny' ? 'bad' : action === 'log' ? 'info' : ''
  return <span className={`pill ${tone}`}>{action ? action.charAt(0).toUpperCase() + action.slice(1) : '—'}</span>
}

function PhaseBadge({ phase }: { phase: string }) {
  const tone = phase.toLowerCase() === 'suggested' ? 'info' : phaseTone(phase)
  return <span className={`pill ${tone}`}>{phase}</span>
}

function CreatePolicyForm({ onSubmit, onCancel, isSubmitting, serverError, initial }: {
  initial: PolicyPrefill
  onSubmit: (data: CreateFlowPolicyRequest) => void
  onCancel: () => void
  isSubmitting: boolean
  serverError?: string
}) {
  const uid = useId()
  const [values, setValues] = useState<PolicyFormValues>({
    name: '',
    sourceService: initial.sourceService,
    destinationService: initial.destinationService,
    port: initial.port,
    intent: initial.sourceService && initial.destinationService ? `Allow ${initial.sourceService} to reach ${initial.destinationService} on port ${initial.port}` : '',
  })
  const [protocol, setProtocol] = useState<string>(initial.protocol)
  const [action, setAction] = useState<PolicyAction>('allow')
  const [submitted, setSubmitted] = useState(false)

  const errors: PolicyFormErrors = submitted ? validatePolicyForm(values) : {}
  const set = (k: keyof PolicyFormValues) => (e: React.ChangeEvent<HTMLInputElement>) => setValues({ ...values, [k]: e.target.value })

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    setSubmitted(true)
    if (Object.keys(validatePolicyForm(values)).length > 0) return
    const req = {
      sourceService: values.sourceService.trim(),
      destinationService: values.destinationService.trim(),
      port: Number(values.port),
      protocol,
      action,
      intent: values.intent.trim(),
      ...(values.name.trim() ? { name: values.name.trim() } : {}),
    }
    onSubmit(req as unknown as CreateFlowPolicyRequest)
  }

  const field = (k: keyof PolicyFormValues, hint?: string) => {
    const hintId = `${uid}-${k}-hint`
    const errId = `${uid}-${k}-err`
    const describedBy = [hint ? hintId : '', errors[k] ? errId : ''].filter(Boolean).join(' ') || undefined
    return {
      'aria-invalid': errors[k] ? true : undefined,
      'aria-describedby': describedBy,
      hintNode: hint ? (
        <small id={hintId} className="faint">
          {hint}
        </small>
      ) : null,
      errNode: errors[k] ? (
        <small id={errId} className="text-bad" role="alert">
          {errors[k]}
        </small>
      ) : null,
    }
  }
  const fName = field('name', 'Optional. Lowercase letters, digits and "-"; generated if left empty. Created in the gateway’s default namespace.')
  const fSrc = field('sourceService', 'Lowercase Kubernetes name, up to 63 characters.')
  const fDst = field('destinationService', 'Lowercase Kubernetes name, up to 63 characters.')
  const fPort = field('port')
  const fIntent = field('intent', `Plain-language purpose, up to ${INTENT_MAX} characters.`)

  return (
    <section className="card span3">
      <p className="eyebrow">NEW</p>
      <h2 className="card-title">Create flow policy</h2>
      <form onSubmit={handleSubmit} className="stack" noValidate>
        {serverError && (
          <div className="warning" role="alert">
            <strong>The gateway rejected this policy.</strong>
            <div>{serverError}</div>
          </div>
        )}
        <div className="formgrid">
          <label className="field">
            <span>Name</span>
            <input type="text" value={values.name} onChange={set('name')} placeholder="e.g., frontend-to-payment" aria-invalid={fName['aria-invalid']} aria-describedby={fName['aria-describedby']} />
            {fName.hintNode}
            {fName.errNode}
          </label>
          <label className="field">
            <span>Source service *</span>
            <input type="text" value={values.sourceService} onChange={set('sourceService')} placeholder="e.g., frontend" required aria-invalid={fSrc['aria-invalid']} aria-describedby={fSrc['aria-describedby']} />
            {fSrc.hintNode}
            {fSrc.errNode}
          </label>
          <label className="field">
            <span>Destination service *</span>
            <input type="text" value={values.destinationService} onChange={set('destinationService')} placeholder="e.g., payment-api" required aria-invalid={fDst['aria-invalid']} aria-describedby={fDst['aria-describedby']} />
            {fDst.hintNode}
            {fDst.errNode}
          </label>
          <label className="field">
            <span>Port *</span>
            <input type="number" min={1} max={65535} step={1} value={values.port} onChange={set('port')} required aria-invalid={fPort['aria-invalid']} aria-describedby={fPort['aria-describedby']} />
            {fPort.errNode}
          </label>
          <label className="field">
            <span>Protocol</span>
            <select value={protocol} onChange={(e) => setProtocol(e.target.value)}>
              {POLICY_PROTOCOLS.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>Action</span>
            <select value={action} onChange={(e) => setAction(e.target.value as PolicyAction)} aria-describedby={`${uid}-action-hint`}>
              <option value="allow">Allow</option>
              <option value="deny">Deny</option>
              <option value="log">Log</option>
            </select>
            <small id={`${uid}-action-hint`} className="faint">
              log = observe only, safe to trial
            </small>
          </label>
          <label className="field span-all">
            <span>Intent *</span>
            <input type="text" value={values.intent} onChange={set('intent')} placeholder="e.g., allow payment processing" maxLength={INTENT_MAX + 50} required aria-invalid={fIntent['aria-invalid']} aria-describedby={fIntent['aria-describedby']} />
            {fIntent.hintNode}
            {fIntent.errNode}
          </label>
        </div>

        <div className="list-row">
          <div className="grow">
            <small>Policy preview</small>
            <b>
              {values.sourceService || '(source)'} → {values.destinationService || '(destination)'}
            </b>
          </div>
          <span className="pill info">
            {protocol}:{values.port || '—'}
          </span>
          <ActionPill action={action} />
        </div>

        <div className="toolbar">
          <button type="submit" disabled={isSubmitting} className="primary">
            {isSubmitting ? 'Creating…' : 'Create policy'}
          </button>
          <button type="button" className="btn-secondary" onClick={onCancel} disabled={isSubmitting}>
            Cancel
          </button>
        </div>
      </form>
    </section>
  )
}
