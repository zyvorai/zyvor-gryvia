// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useId, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import type { SecurityPolicy } from '@/lib/api'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import { TableCaption } from '@/components/TableCaption'
import Modal from '@/components/Modal'
import ConfirmDialog from '@/components/ConfirmDialog'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { phaseTone } from '@/lib/phase'
import { formatDate, formatNumber, formatRelative } from '@/lib/format'
import { errorMessage } from '@/lib/errors'
import { notify } from '@/lib/notify'
import { applySearch } from '@/lib/tableState'
import { severityTone } from '@/lib/network'
import {
  RULE_CATALOG,
  SENSITIVITIES,
  activeRuleTypes,
  buildSecurityRequest,
  countActivePolicies,
  defaultRuleDrafts,
  policyCounters,
  ruleName,
  validateNamespaceChip,
  validateSecurityForm,
  type RuleDraft,
  type SecurityFormErrors,
} from '@/lib/security'

export default function SecurityOverview() {
  useDocumentTitle('Security')
  const [creating, setCreating] = useState(false)
  const alertsQ = useQuery({ queryKey: ['securityAlerts'], queryFn: api.getSecurityAlertsStatus, refetchInterval: 10000 })
  const policiesQ = useQuery({ queryKey: ['securityPolicies'], queryFn: api.getSecurityPolicies, refetchInterval: 30000 })

  const alerts = alertsQ.data?.items
  const eventSource = alertsQ.data ? alertsQ.data.eventSource !== false : undefined
  const policies = policiesQ.data
  const critical = eventSource && alerts ? alerts.filter((a) => a.severity === 'critical').length : undefined
  const high = eventSource && alerts ? alerts.filter((a) => a.severity === 'high').length : undefined
  const activeRules = policies ? activeRuleTypes(policies) : undefined
  const totalRules = policies ? policies.reduce((n, p) => n + ((p.status?.phase ?? '').toLowerCase() === 'active' ? (p.spec.detectionRules?.filter((r) => r.enabled).length ?? 0) : 0), 0) : undefined

  let headline: string | undefined
  let headlineTone: 'ok' | 'bad' | undefined
  if (alertsQ.data) {
    if (eventSource === false) headline = 'No event source connected: alerts cannot be shown.'
    else if (critical && critical > 0) {
      headline = `Threats detected: ${critical} critical alert${critical === 1 ? '' : 's'}.`
      headlineTone = 'bad'
    } else {
      headline = 'All clear: no critical alerts.'
      headlineTone = 'ok'
    }
  }

  return (
    <>
      <PageHero eyebrow="Security" title="Security detection." lede="eBPF threat-detection alerts, policies and rule coverage." tint={critical && critical > 0 ? 'red' : undefined} />

      <div className="grid">
        <div className="toolbar span3">
          <button type="button" className="primary" onClick={() => setCreating(true)}>
            Create security policy
          </button>
          <button
            type="button"
            className="btn-secondary"
            onClick={() => {
              void alertsQ.refetch()
              void policiesQ.refetch()
            }}
            disabled={alertsQ.isFetching || policiesQ.isFetching}
            aria-busy={alertsQ.isFetching || policiesQ.isFetching}
          >
            {alertsQ.isFetching || policiesQ.isFetching ? 'Refreshing…' : 'Refresh'}
          </button>
        </div>

        {alertsQ.isLoading ? (
          <section className="card span3" aria-label="Live summary">
            <Skeleton rows={2} />
          </section>
        ) : alertsQ.isError && !alertsQ.data ? (
          <div className="span3">
            <ErrorState title="Could not load security alerts." error={alertsQ.error} onRetry={() => alertsQ.refetch()} retrying={alertsQ.isFetching} />
          </div>
        ) : (
          <PagePulse
            updatedAt={alertsQ.dataUpdatedAt}
            error={alertsQ.isError ? errorMessage(alertsQ.error) : undefined}
            headline={headline}
            tone={headlineTone}
            figures={[
              { label: 'critical alerts', value: critical ?? '—', tone: critical && critical > 0 ? 'bad' : undefined },
              { label: 'high alerts', value: high ?? '—', tone: high && high > 0 ? 'warn' : undefined },
              { label: 'active policies', value: policies ? countActivePolicies(policies) : undefined },
              { label: 'active rules', value: totalRules },
            ]}
          />
        )}

        <section className="card span2">
          <p className="eyebrow">ALERTS{alertsQ.data && eventSource ? ` · ${alerts?.length ?? 0} TOTAL` : ''}</p>
          <h2 className="card-title">Recent security alerts</h2>
          {alertsQ.isLoading ? (
            <Skeleton rows={4} />
          ) : alertsQ.isError && !alertsQ.data ? (
            <ErrorState title="Could not load security alerts." error={alertsQ.error} onRetry={() => alertsQ.refetch()} retrying={alertsQ.isFetching} />
          ) : eventSource === false ? (
            <EmptyState
              title="No event source connected"
              action={
                <button type="button" className="btn-secondary" onClick={() => alertsQ.refetch()} disabled={alertsQ.isFetching}>
                  {alertsQ.isFetching ? 'Checking…' : 'Check again'}
                </button>
              }
            >
              Alerts come from the security operator&apos;s eBPF event stream. Until it is connected, an empty list does not mean your workloads are safe.
            </EmptyState>
          ) : !alerts || alerts.length === 0 ? (
            <EmptyState title="No security alerts">The security operator's event source is connected and has reported no alerts.</EmptyState>
          ) : (
            <div className="table-wrap">
              <table>
                <TableCaption>Recent security alerts</TableCaption>
                <thead>
                  <tr>
                    <th scope="col">Severity</th>
                    <th scope="col">Event type</th>
                    <th scope="col">Process</th>
                    <th scope="col">Path</th>
                    <th scope="col">Time</th>
                  </tr>
                </thead>
                <tbody>
                  {alerts.slice(0, 12).map((alert, idx) => (
                    <tr key={`${alert.type}-${idx}`}>
                      <td>
                        <span className={`pill ${severityTone(alert.severity)}`}>{alert.severity}</span>
                      </td>
                      <td title={alert.type}>{ruleName(alert.type)}</td>
                      <td className="mono muted">{alert.process || '—'}</td>
                      <td className="mono faint">{alert.path || '—'}</td>
                      <td className="faint" title={alert.timestamp ? formatDate(alert.timestamp) : undefined}>
                        {alert.timestamp ? `${formatDate(alert.timestamp)} · ${formatRelative(alert.timestamp)}` : '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {policies && policies.length > 0 && <CounterSummary policies={policies} />}
        </section>

        <section className="card">
          <p className="eyebrow">CONFIGURATION</p>
          <h2 className="card-title">Security policies</h2>
          {policiesQ.isLoading ? (
            <Skeleton rows={3} />
          ) : policiesQ.isError && !policies ? (
            <ErrorState title="Could not load security policies." error={policiesQ.error} onRetry={() => policiesQ.refetch()} retrying={policiesQ.isFetching} />
          ) : !policies || policies.length === 0 ? (
            <EmptyState title="No security policies configured" action={<button type="button" className="primary" onClick={() => setCreating(true)}>Create security policy</button>}>
              Security policies are GryviaSecurityPolicy resources evaluated by the security operator. Without one, no detection rules run and no alerts are produced.
            </EmptyState>
          ) : (
            <PolicyList policies={policies} />
          )}
        </section>

        <section className="card span3">
          <p className="eyebrow">COVERAGE</p>
          <h2 className="card-title">Detection rules</h2>
          {policiesQ.isLoading ? (
            <Skeleton rows={5} />
          ) : policiesQ.isError && !policies ? (
            <ErrorState title="Could not load detection coverage." error={policiesQ.error} onRetry={() => policiesQ.refetch()} retrying={policiesQ.isFetching} />
          ) : (
            RULE_CATALOG.map((rule) => {
              const isEnabled = activeRules?.has(rule.type) ?? false
              return (
                <div key={rule.type} className="list-row">
                  <span className={`dot ${isEnabled ? 'ok' : 'warn'}`} aria-hidden="true" />
                  <div className="grow">
                    <b>{rule.name}</b>
                    <small>{rule.description}</small>
                  </div>
                  <span className={`pill ${isEnabled ? 'ok' : 'warn'}`}>{isEnabled ? 'Active' : 'Not covered'}</span>
                </div>
              )
            })
          )}
        </section>
      </div>

      {creating && <CreatePolicyModal onClose={() => setCreating(false)} />}
    </>
  )
}

function CounterSummary({ policies }: { policies: SecurityPolicy[] }) {
  const c = policyCounters(policies)
  return (
    <p className="faint mt-10">
      Policy counters: {formatNumber(c.alerts)} alert{c.alerts === 1 ? '' : 's'} triggered
      {c.lastAlert && (
        <>
          {' '}
          · last alert <span title={formatDate(c.lastAlert)}>{formatRelative(c.lastAlert)}</span>
        </>
      )}
      {c.byType.length > 0 && <> · {c.byType.map(([k, v]) => `${ruleName(k)} ${v}`).join(', ')}</>}
    </p>
  )
}

function PolicyList({ policies }: { policies: SecurityPolicy[] }) {
  const [q, setQ] = useState('')
  const [open, setOpen] = useState<string | null>(null)
  const shown = applySearch(policies, q, (p) => `${p.metadata.name} ${(p.spec.targetNamespaces ?? []).join(' ')} ${p.status?.phase ?? ''} ${(p.spec.detectionRules ?? []).map((r) => ruleName(r.type)).join(' ')}`)
  return (
    <div className="stack">
      <label className="field">
        <span>Search policies</span>
        <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Name, namespace, rule…" />
      </label>
      <p className="faint" role="status">
        Showing {shown.length} of {policies.length}
      </p>
      {shown.length === 0 ? (
        <EmptyState
          title="No policy matches"
          action={
            <button type="button" className="btn-secondary" onClick={() => setQ('')}>
              Clear search
            </button>
          }
        />
      ) : (
        shown.map((policy) => <PolicyRow key={policy.metadata.name} policy={policy} open={open === policy.metadata.name} onToggle={() => setOpen(open === policy.metadata.name ? null : policy.metadata.name)} />)
      )}
    </div>
  )
}

function PolicyRow({ policy, open, onToggle }: { policy: SecurityPolicy; open: boolean; onToggle: () => void }) {
  const phase = policy.status?.phase || 'Pending'
  const rules = policy.spec.detectionRules ?? []
  const enabled = rules.filter((r) => r.enabled)
  const namespaces = policy.spec.targetNamespaces ?? []
  const counts = Object.entries(policy.status?.detectionCounts ?? {})
  const last = policy.status?.lastAlert
  const detailId = `policy-detail-${policy.metadata.name}`
  return (
    <div>
      <div className="list-row">
        <div className="grow">
          <b>{policy.metadata.name}</b>
          <small>
            {enabled.length} rule{enabled.length === 1 ? '' : 's'} enabled · {policy.status?.alertsTriggered ?? 0} alerts
            {last && (
              <>
                {' '}
                · last <span title={formatDate(last)}>{formatRelative(last)}</span>
              </>
            )}
          </small>
        </div>
        {policy.spec.autoBlock && (
          <span className="pill warn" title="Matching processes are blocked automatically">
            Auto-block
          </span>
        )}
        <span className={`pill ${phaseTone(phase)}`}>{phase}</span>
        <button type="button" className="btn-secondary" aria-expanded={open} aria-controls={open ? detailId : undefined} onClick={onToggle}>
          {open ? 'Hide details' : 'Show details'}
          <span className="sr-only"> of {policy.metadata.name}</span>
        </button>
      </div>
      {open && (
        <div id={detailId} className="stack">
          <small>
            <b>Namespaces:</b> {namespaces.length > 0 ? namespaces.join(', ') : 'none selected'}
          </small>
          <small>
            <b>Alert webhook:</b> {policy.spec.alertWebhook || 'none'}
          </small>
          <div className="table-wrap">
            <table>
              <TableCaption>{`Detection rules of ${policy.metadata.name}`}</TableCaption>
              <thead>
                <tr>
                  <th scope="col">Rule</th>
                  <th scope="col">State</th>
                  <th scope="col">Sensitivity</th>
                  <th scope="col" className="num">Detections</th>
                </tr>
              </thead>
              <tbody>
                {rules.map((r) => (
                  <tr key={r.type}>
                    <td>{ruleName(r.type)}</td>
                    <td>
                      <span className={`pill ${r.enabled ? 'ok' : ''}`}>{r.enabled ? 'Enabled' : 'Disabled'}</span>
                    </td>
                    <td>{r.sensitivity || '—'}</td>
                    <td className="num">{policy.status?.detectionCounts?.[r.type] ?? 0}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <small className="faint">
            {policy.status?.activeDetections ?? 0} active detection{(policy.status?.activeDetections ?? 0) === 1 ? '' : 's'} · {policy.status?.alertsTriggered ?? 0} alert{(policy.status?.alertsTriggered ?? 0) === 1 ? '' : 's'} triggered
            {counts.length > 0 && ` · ${counts.map(([k, v]) => `${ruleName(k)} ${v}`).join(', ')}`}
          </small>
        </div>
      )}
    </div>
  )
}

function CreatePolicyModal({ onClose }: { onClose: () => void }) {
  const uid = useId()
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [namespaces, setNamespaces] = useState<string[]>([])
  const [nsInput, setNsInput] = useState('')
  const [nsError, setNsError] = useState<string | undefined>()
  const [rules, setRules] = useState<RuleDraft[]>(defaultRuleDrafts)
  const [webhook, setWebhook] = useState('')
  const [autoBlock, setAutoBlock] = useState(false)
  const [submitted, setSubmitted] = useState(false)
  const [confirming, setConfirming] = useState(false)

  const values = { name, namespaces, rules, webhook, autoBlock }
  const errors: SecurityFormErrors = submitted ? validateSecurityForm(values) : {}
  const dirty = name !== '' || namespaces.length > 0 || nsInput !== '' || webhook !== '' || autoBlock || rules.some((r) => r.enabled)

  const mutation = useMutation({
    mutationFn: api.createSecurityPolicy,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['securityPolicies'] })
      notify.success('Security policy created')
      onClose()
    },
    onError: (err) => {
      setConfirming(false)
      notify.error('Could not create security policy', err)
    },
  })

  const addNamespace = () => {
    const v = nsInput.trim()
    if (!v) return
    const err = validateNamespaceChip(v, namespaces)
    setNsError(err)
    if (err) return
    setNamespaces([...namespaces, v])
    setNsInput('')
  }
  const setRule = (type: string, patch: Partial<RuleDraft>) => setRules(rules.map((r) => (r.type === type ? { ...r, ...patch } : r)))

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setSubmitted(true)
    if (Object.keys(validateSecurityForm(values)).length > 0) return
    if (autoBlock) setConfirming(true)
    else mutation.mutate(buildSecurityRequest(values))
  }

  const nameErr = errors.name
  const nsErr = nsError ?? errors.namespaces
  const hookErr = errors.webhook
  const describe = (...ids: Array<string | false | undefined>) => ids.filter(Boolean).join(' ') || undefined

  return (
    <>
      <Modal title="Create security policy" onClose={onClose} dirty={dirty || mutation.isPending}>
        <form onSubmit={submit} className="stack" noValidate>
          {mutation.isError && (
            <div className="warning" role="alert">
              <strong>The gateway rejected this policy.</strong>
              <div>{errorMessage(mutation.error)}</div>
            </div>
          )}

          <label className="field">
            <span>Name *</span>
            <input type="text" value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g., gpu-workloads" required aria-invalid={nameErr ? true : undefined} aria-describedby={describe(`${uid}-name-hint`, nameErr && `${uid}-name-err`)} />
            <small id={`${uid}-name-hint`} className="faint">
              Lowercase letters, digits and &quot;-&quot;, up to 63 characters.
            </small>
            {nameErr && (
              <small id={`${uid}-name-err`} className="text-bad" role="alert">
                {nameErr}
              </small>
            )}
          </label>

          <div className="field">
            <label htmlFor={`${uid}-ns`}>Target namespaces *</label>
            <div className="toolbar">
              <input
                id={`${uid}-ns`}
                type="text"
                value={nsInput}
                onChange={(e) => {
                  setNsInput(e.target.value)
                  setNsError(undefined)
                }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ',') {
                    e.preventDefault()
                    addNamespace()
                  }
                }}
                placeholder="e.g., ml-training"
                aria-invalid={nsErr ? true : undefined}
                aria-describedby={describe(`${uid}-ns-hint`, nsErr && `${uid}-ns-err`)}
              />
              <button type="button" className="btn-secondary" onClick={addNamespace}>
                Add namespace
              </button>
            </div>
            <small id={`${uid}-ns-hint`} className="faint">
              Press Enter to add. Each must be a Kubernetes name.
            </small>
            {nsErr && (
              <small id={`${uid}-ns-err`} className="text-bad" role="alert">
                {nsErr}
              </small>
            )}
            {namespaces.length > 0 && (
              <div className="chips" role="list" aria-label="Selected namespaces">
                {namespaces.map((n) => (
                  <span key={n} role="listitem">
                    {n}{' '}
                    <button type="button" aria-label={`Remove namespace ${n}`} onClick={() => setNamespaces(namespaces.filter((x) => x !== n))}>
                      ×
                    </button>
                  </span>
                ))}
              </div>
            )}
          </div>

          <fieldset className="field" aria-invalid={errors.rules ? true : undefined} aria-describedby={describe(errors.rules && `${uid}-rules-err`)}>
            <legend>Detection rules *</legend>
            <div className="table-wrap">
              <table>
                <TableCaption>Detection rules for this policy</TableCaption>
                <thead>
                  <tr>
                    <th scope="col">Rule</th>
                    <th scope="col">Enabled</th>
                    <th scope="col">Sensitivity</th>
                  </tr>
                </thead>
                <tbody>
                  {rules.map((r) => {
                    const cat = RULE_CATALOG.find((c) => c.type === r.type)
                    return (
                      <tr key={r.type}>
                        <td>
                          {ruleName(r.type)}
                          {cat && <small className="faint"> {cat.description}</small>}
                        </td>
                        <td>
                          <input type="checkbox" role="switch" checked={r.enabled} onChange={(e) => setRule(r.type, { enabled: e.target.checked })} aria-label={`Enable ${ruleName(r.type)}`} />
                        </td>
                        <td>
                          <select value={r.sensitivity} onChange={(e) => setRule(r.type, { sensitivity: e.target.value })} aria-label={`Sensitivity for ${ruleName(r.type)}`} disabled={!r.enabled}>
                            {SENSITIVITIES.map((sv) => (
                              <option key={sv} value={sv}>
                                {sv}
                              </option>
                            ))}
                          </select>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
            {errors.rules && (
              <small id={`${uid}-rules-err`} className="text-bad" role="alert">
                {errors.rules}
              </small>
            )}
          </fieldset>

          <label className="field">
            <span>Alert webhook URL</span>
            <input type="url" value={webhook} onChange={(e) => setWebhook(e.target.value)} placeholder="https://hooks.example.com/alerts" aria-invalid={hookErr ? true : undefined} aria-describedby={describe(`${uid}-hook-hint`, hookErr && `${uid}-hook-err`)} />
            <small id={`${uid}-hook-hint`} className="faint">
              Optional. Alerts are posted here as JSON; must be http(s).
            </small>
            {hookErr && (
              <small id={`${uid}-hook-err`} className="text-bad" role="alert">
                {hookErr}
              </small>
            )}
          </label>

          <div className="field">
            <label className="row">
              <input type="checkbox" role="switch" checked={autoBlock} onChange={(e) => setAutoBlock(e.target.checked)} aria-describedby={`${uid}-block-hint`} />
              <span>Auto-block matching processes</span>
            </label>
            <small id={`${uid}-block-hint`} className={autoBlock ? 'text-bad' : 'faint'}>
              Warning: with auto-block on, processes that trigger an enabled rule are killed without review. False positives will interrupt workloads. Start with it off.
            </small>
          </div>

          <div className="toolbar">
            <button type="submit" className="primary" disabled={mutation.isPending}>
              {mutation.isPending ? 'Creating…' : 'Create policy'}
            </button>
            <button type="button" className="btn-secondary" onClick={onClose} disabled={mutation.isPending}>
              Cancel
            </button>
          </div>
        </form>
      </Modal>

      {confirming && (
        <ConfirmDialog
          title={`Create ${name.trim()} with auto-block?`}
          confirmLabel="Create with auto-block"
          busy={mutation.isPending}
          onCancel={() => setConfirming(false)}
          onConfirm={() => mutation.mutate(buildSecurityRequest(values))}
        >
          Processes matching {rules.filter((r) => r.enabled).map((r) => ruleName(r.type)).join(', ')} in {namespaces.join(', ')} will be blocked automatically. A false positive stops a real workload. Continue?
        </ConfirmDialog>
      )}
    </>
  )
}
