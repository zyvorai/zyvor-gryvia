// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useId, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { api } from '@/lib/api'
import type { ServiceGraphNode, ServiceGraphEdge, NetworkAnomaly, TraceSession } from '@/lib/api'
import Modal from '@/components/Modal'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import { TableCaption } from '@/components/TableCaption'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import { phaseTone } from '@/lib/phase'
import { formatDate, formatRelative } from '@/lib/format'
import { errorMessage } from '@/lib/errors'
import { notify } from '@/lib/notify'
import {
  TRACE_LEVELS,
  buildTraceDuration,
  edgeGeometry,
  flowsUrlForEdge,
  flowsUrlForService,
  formatCaptureLevel,
  graphFocusModel,
  healthKind,
  parseTraceTarget,
  severityTone,
  twoWayEdges,
  validateTraceForm,
  verdictKind,
  type GraphFocus,
  type HealthKind,
  type TraceFormErrors,
  type TraceFormValues,
  type VerdictKind,
} from '@/lib/network'

export default function NetworkOverview() {
  useDocumentTitle('Network')
  const [params, setParams] = useSearchParams()
  const traceTarget = parseTraceTarget(params)
  const [creating, setCreating] = useState(false)
  const [viewing, setViewing] = useState<TraceSession | null>(null)
  const insightsQ = useQuery({ queryKey: ['trafficInsights'], queryFn: api.getTrafficInsights, refetchInterval: 15000 })
  const graphQ = useQuery({ queryKey: ['serviceGraph'], queryFn: api.getServiceGraph, refetchInterval: 30000 })
  const anomaliesQ = useQuery({ queryKey: ['networkAnomalies'], queryFn: api.getNetworkAnomalies, refetchInterval: 15000 })
  const tracesQ = useQuery({ queryKey: ['traceSessions'], queryFn: api.getTraceSessions, refetchInterval: 10000 })

  const insights = insightsQ.data
  const showCreate = creating || traceTarget !== null
  const closeCreate = () => {
    setCreating(false)
    if (traceTarget !== null) {
      setParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          next.delete('trace')
          return next
        },
        { replace: true },
      )
    }
  }
  const serviceNames = [...new Set((graphQ.data?.nodes ?? []).map((n) => n.label))].sort((a, b) => a.localeCompare(b))
  const anomalyCount = insights?.anomaliesDetected
  const refreshing = insightsQ.isFetching || graphQ.isFetching || anomaliesQ.isFetching || tracesQ.isFetching
  const refreshAll = () => {
    void insightsQ.refetch()
    void graphQ.refetch()
    void anomaliesQ.refetch()
    void tracesQ.refetch()
  }
  const pulseError = insightsQ.isError ? errorMessage(insightsQ.error) : undefined

  return (
    <>
      <PageHero eyebrow="Network" title="Network intelligence." lede="Service dependencies, anomalies and trace sessions across the mesh." />

      <div className="grid">
        <div className="toolbar span3">
          <Link to="/network/policies" className="buttonlike primary">
            Policies
          </Link>
          <Link to="/network/flows" className="buttonlike btn-secondary">
            View flows
          </Link>
          <button type="button" className="btn-secondary" onClick={refreshAll} disabled={refreshing} aria-busy={refreshing}>
            {refreshing ? 'Refreshing…' : 'Refresh'}
          </button>
        </div>

        {insightsQ.isLoading ? (
          <section className="card span3" aria-label="Live summary">
            <Skeleton rows={2} />
          </section>
        ) : insightsQ.isError && !insights ? (
          <div className="span3">
            <ErrorState title="Could not load network insights." error={insightsQ.error} onRetry={() => insightsQ.refetch()} retrying={insightsQ.isFetching} />
          </div>
        ) : (
          <PagePulse
            updatedAt={insightsQ.dataUpdatedAt}
            error={pulseError}
            headline={
              anomalyCount === undefined
                ? undefined
                : anomalyCount > 0
                  ? `${anomalyCount} anomal${anomalyCount === 1 ? 'y' : 'ies'} detected across the mesh.`
                  : 'No anomalies detected across the mesh.'
            }
            tone={anomalyCount && anomalyCount > 0 ? 'warn' : undefined}
            figures={[
              { label: 'Observed connections', value: insights?.activeFlows },
              { label: 'Policies enforced', value: insights?.policiesEnforced },
              { label: 'Anomalies detected', value: anomalyCount, tone: anomalyCount && anomalyCount > 0 ? 'warn' : undefined },
              { label: 'Trace sessions', value: insights?.traceSessions },
            ]}
          />
        )}

        <section className="card span2">
          <p className="eyebrow">TOPOLOGY</p>
          <h2 className="card-title">Service dependency graph</h2>
          {graphQ.isLoading ? (
            <Skeleton rows={4} />
          ) : graphQ.isError && !graphQ.data ? (
            <ErrorState title="Could not load the service graph." error={graphQ.error} onRetry={() => graphQ.refetch()} retrying={graphQ.isFetching} />
          ) : (
            <>
              {graphQ.isError && <p className="faint" role="status">Showing the last loaded graph; refresh failed: {errorMessage(graphQ.error)}</p>}
              <ServiceGraphView nodes={graphQ.data?.nodes || []} edges={graphQ.data?.edges || []} />
            </>
          )}
        </section>

        <section className="card">
          <p className="eyebrow">CAPTURE</p>
          <h2 className="card-title">Active traces</h2>
          <div className="toolbar">
            <button type="button" className="primary" onClick={() => setCreating(true)}>
              Create trace
            </button>
          </div>
          {tracesQ.isLoading ? (
            <Skeleton rows={3} />
          ) : tracesQ.isError && !tracesQ.data ? (
            <ErrorState title="Could not load trace sessions." error={tracesQ.error} onRetry={() => tracesQ.refetch()} retrying={tracesQ.isFetching} />
          ) : !tracesQ.data || tracesQ.data.length === 0 ? (
            <EmptyState title="No active trace sessions">
              A trace captures flows for one service for a fixed time. Use Create trace to start one; the network-intelligence operator runs it and reports the result here.
            </EmptyState>
          ) : (
            <>
              {tracesQ.data.slice(0, 8).map((trace) => (
                <TraceRow key={trace.metadata.name} trace={trace} onOpen={() => setViewing(trace)} />
              ))}
              {tracesQ.data.length > 8 && <p className="faint">{tracesQ.data.length - 8} more not shown.</p>}
            </>
          )}
        </section>

        <section className="card span3">
          <p className="eyebrow">ANOMALIES{anomaliesQ.data ? ` · ${anomaliesQ.data.length} TOTAL` : ''}</p>
          <h2 className="card-title">Recent anomalies</h2>
          {anomaliesQ.isLoading ? (
            <Skeleton rows={3} />
          ) : anomaliesQ.isError && !anomaliesQ.data ? (
            <ErrorState title="Could not load anomalies." error={anomaliesQ.error} onRetry={() => anomaliesQ.refetch()} retrying={anomaliesQ.isFetching} />
          ) : !anomaliesQ.data || anomaliesQ.data.length === 0 ? (
            <EmptyState title="No anomalies detected" action={<Link to="/network/flows" className="buttonlike btn-secondary">View flows</Link>}>
              Anomalies are raised by the network-intelligence operator from observed traffic.
            </EmptyState>
          ) : (
            <div className="table-wrap">
              <table>
                <TableCaption>Recent network anomalies</TableCaption>
                <thead>
                  <tr>
                    <th scope="col">Severity</th>
                    <th scope="col">Service</th>
                    <th scope="col">Type</th>
                    <th scope="col">Description</th>
                    <th scope="col">Detected</th>
                  </tr>
                </thead>
                <tbody>
                  {anomaliesQ.data.slice(0, 8).map((a) => (
                    <AnomalyRow key={a.metadata.name} anomaly={a} />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      </div>

      {showCreate && <CreateTraceModal services={serviceNames} initialTarget={traceTarget ?? ''} onClose={closeCreate} />}
      {viewing && <TraceDetailModal trace={viewing} onClose={() => setViewing(null)} />}
    </>
  )
}

function TraceRow({ trace, onOpen }: { trace: TraceSession; onOpen: () => void }) {
  const phase = trace.status?.phase || 'Pending'
  return (
    <div
      className="list-row"
      role="button"
      tabIndex={0}
      aria-label={`Trace of ${trace.spec.targetService}, ${phase}. Open details.`}
      onClick={onOpen}
      onKeyDown={(e) => {
        if ((e.key === 'Enter' || e.key === ' ') && e.target === e.currentTarget) {
          e.preventDefault()
          onOpen()
        }
      }}
    >
      <div className="grow">
        <b>{trace.spec.targetService}</b>
        <small>
          Level: {formatCaptureLevel(trace.spec.captureLevel)} · Duration: {trace.spec.duration}
          {trace.status?.flowsCaptured !== undefined && ` · ${trace.status.flowsCaptured} flows captured`}
        </small>
      </div>
      <span className={`pill ${phaseTone(phase)}`}>{phase}</span>
    </div>
  )
}

function TraceDetailModal({ trace, onClose }: { trace: TraceSession; onClose: () => void }) {
  const created = trace.metadata.creationTimestamp
  const rows: Array<[string, string]> = [
    ['Session', trace.metadata.name],
    ['Target service', trace.spec.targetService],
    ['Namespace', trace.spec.namespace || '—'],
    ['Capture level', formatCaptureLevel(trace.spec.captureLevel)],
    ['Duration', trace.spec.duration || '—'],
    ['Phase', trace.status?.phase || 'Pending'],
    ['Flows captured', trace.status?.flowsCaptured !== undefined ? String(trace.status.flowsCaptured) : 'not reported yet'],
    ['Created', created ? `${formatDate(created)} · ${formatRelative(created)}` : '—'],
  ]
  return (
    <Modal title={`Trace of ${trace.spec.targetService}`} onClose={onClose}>
      <div className="stack">
        {rows.map(([k, v]) => (
          <div key={k} className="list-row">
            <span className="grow faint">{k}</span>
            <span className="mono">{v}</span>
          </div>
        ))}
        <div className="toolbar">
          <Link to={flowsUrlForService(trace.spec.targetService)} className="buttonlike btn-secondary">
            View connections
          </Link>
          <button type="button" className="btn-secondary" data-autofocus="" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </Modal>
  )
}

const OTHER = '__other__'

function CreateTraceModal({ services, initialTarget, onClose }: { services: string[]; initialTarget: string; onClose: () => void }) {
  const uid = useId()
  const queryClient = useQueryClient()
  const known = services.includes(initialTarget)
  const [choice, setChoice] = useState(services.length === 0 || (initialTarget && !known) ? OTHER : known ? initialTarget : services[0])
  const [custom, setCustom] = useState(known ? '' : initialTarget)
  const [values, setValues] = useState<Omit<TraceFormValues, 'targetService'>>({ namespace: 'default', captureLevel: 'l4', durationAmount: '5', durationUnit: 'm' })
  const [submitted, setSubmitted] = useState(false)

  const form: TraceFormValues = { ...values, targetService: choice === OTHER ? custom : choice }
  const errors: TraceFormErrors = submitted ? validateTraceForm(form) : {}
  const level = TRACE_LEVELS.find((l) => l.value === values.captureLevel)
  const dirty = custom !== '' || values.namespace !== 'default' || values.captureLevel !== 'l4' || values.durationAmount !== '5' || values.durationUnit !== 'm'

  const mutation = useMutation({
    mutationFn: api.createTraceSession,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['traceSessions'] })
      queryClient.invalidateQueries({ queryKey: ['trafficInsights'] })
      notify.success('Trace requested')
      onClose()
    },
    onError: (err) => notify.error('Could not create trace', err),
  })

  const props = (k: keyof TraceFormValues, hint?: string) => {
    const err = errors[k]
    return {
      'aria-invalid': err ? true : undefined,
      'aria-describedby': [hint ? `${uid}-${k}-hint` : '', err ? `${uid}-${k}-err` : ''].filter(Boolean).join(' ') || undefined,
      hint: hint ? (
        <small id={`${uid}-${k}-hint`} className="faint">
          {hint}
        </small>
      ) : null,
      err: err ? (
        <small id={`${uid}-${k}-err`} className="text-bad" role="alert">
          {err}
        </small>
      ) : null,
    }
  }
  const pTarget = props('targetService', 'Kubernetes name of the service to capture.')
  const pNs = props('namespace', 'Namespace the service runs in.')
  const pLevel = props('captureLevel', level?.help)
  const pAmount = props('durationAmount', '1 to 9999. The gateway names the session automatically.')
  const pUnit = props('durationUnit')

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setSubmitted(true)
    if (Object.keys(validateTraceForm(form)).length > 0) return
    mutation.mutate({
      targetService: form.targetService.trim(),
      namespace: form.namespace.trim(),
      captureLevel: form.captureLevel,
      duration: buildTraceDuration(form.durationAmount, form.durationUnit) as string,
    })
  }

  return (
    <Modal title="Create trace" onClose={onClose} dirty={dirty || mutation.isPending}>
      <form onSubmit={submit} className="stack" noValidate>
        {mutation.isError && (
          <div className="warning" role="alert">
            <strong>The gateway rejected this trace.</strong>
            <div>{errorMessage(mutation.error)}</div>
          </div>
        )}
        <div className="formgrid">
          {services.length > 0 && (
            <label className="field span-all">
              <span>Target service</span>
              <select value={choice} onChange={(e) => setChoice(e.target.value)}>
                {services.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
                <option value={OTHER}>Other (type a name)…</option>
              </select>
            </label>
          )}
          {choice === OTHER && (
            <label className="field span-all">
              <span>{services.length > 0 ? 'Service name' : 'Target service'}</span>
              <input type="text" value={custom} onChange={(e) => setCustom(e.target.value)} placeholder="e.g., payment-api" aria-invalid={pTarget['aria-invalid']} aria-describedby={pTarget['aria-describedby']} required />
              {pTarget.hint}
              {pTarget.err}
            </label>
          )}
          <label className="field">
            <span>Namespace</span>
            <input type="text" value={values.namespace} onChange={(e) => setValues({ ...values, namespace: e.target.value })} aria-invalid={pNs['aria-invalid']} aria-describedby={pNs['aria-describedby']} required />
            {pNs.hint}
            {pNs.err}
          </label>
          <label className="field">
            <span>Capture level</span>
            <select value={values.captureLevel} onChange={(e) => setValues({ ...values, captureLevel: e.target.value })} aria-invalid={pLevel['aria-invalid']} aria-describedby={pLevel['aria-describedby']}>
              {TRACE_LEVELS.map((l) => (
                <option key={l.value} value={l.value}>
                  {l.label}
                </option>
              ))}
            </select>
            {pLevel.hint}
            {pLevel.err}
          </label>
          <label className="field">
            <span>Duration</span>
            <input type="number" min={1} max={9999} step={1} value={values.durationAmount} onChange={(e) => setValues({ ...values, durationAmount: e.target.value })} aria-invalid={pAmount['aria-invalid']} aria-describedby={pAmount['aria-describedby']} required />
            {pAmount.hint}
            {pAmount.err}
          </label>
          <label className="field">
            <span>Unit</span>
            <select value={values.durationUnit} onChange={(e) => setValues({ ...values, durationUnit: e.target.value })} aria-invalid={pUnit['aria-invalid']} aria-describedby={pUnit['aria-describedby']}>
              <option value="s">seconds</option>
              <option value="m">minutes</option>
              <option value="h">hours</option>
            </select>
            {pUnit.err}
          </label>
        </div>
        <div className="toolbar">
          <button type="submit" className="primary" disabled={mutation.isPending}>
            {mutation.isPending ? 'Creating…' : 'Create trace'}
          </button>
          <button type="button" className="btn-secondary" onClick={onClose} disabled={mutation.isPending}>
            Cancel
          </button>
        </div>
      </form>
    </Modal>
  )
}

function AnomalyRow({ anomaly }: { anomaly: NetworkAnomaly }) {
  const at = anomaly.spec.detectedAt
  return (
    <tr>
      <td>
        <SeverityBadge severity={anomaly.spec.severity} />
      </td>
      <td>{anomaly.spec.service}</td>
      <td className="muted">{anomaly.spec.type}</td>
      <td className="muted">{anomaly.spec.description || '—'}</td>
      <td className="faint" title={at ? formatDate(at) : undefined}>
        {at ? formatRelative(at) : '—'}
      </td>
    </tr>
  )
}

// --- Graph ---

const NODE_W = 140
const NODE_H = 56
const COL_GAP = 180
const ROW_GAP = 90
const PAD = 30
const EDGE_OFFSET = 7

const HEALTH_STROKE: Record<HealthKind, string> = {
  healthy: 'var(--accent-green)',
  warning: 'var(--accent-amber)',
  critical: 'var(--danger)',
  unknown: 'var(--text-faint)',
}
const HEALTH_LABEL: Record<HealthKind, string> = { healthy: 'Healthy', warning: 'Warning', critical: 'Critical', unknown: 'Unknown' }

const EDGE_STYLE: Record<VerdictKind, { color: string; marker: string; dash?: string; label: string }> = {
  good: { color: 'var(--accent-green)', marker: 'arrow-good', label: 'Forwarded / allowed' },
  drop: { color: 'var(--danger)', marker: 'arrow-drop', label: 'Dropped / denied' },
  error: { color: 'var(--accent-amber)', marker: 'arrow-error', label: 'Error' },
  unknown: { color: 'var(--text-faint)', marker: 'arrow-unknown', dash: '4 3', label: 'Unknown' },
}

function latencyLabel(latency?: string): string | null {
  const l = (latency ?? '').trim()
  return l && l !== '-' ? `p99 ${l}` : null
}

function ServiceGraphView({ nodes, edges }: { nodes: ServiceGraphNode[]; edges: ServiceGraphEdge[] }) {
  if (nodes.length === 0) {
    return (
      <EmptyState
        title="No service graph yet"
        action={
          <Link to="/network/flows" className="buttonlike btn-secondary">
            View flows
          </Link>
        }
      >
        Requires the network-intelligence operator to populate GryviaServiceGraph.
      </EmptyState>
    )
  }

  return <InteractiveGraph nodes={nodes} edges={edges} />
}

function InteractiveGraph({ nodes, edges }: { nodes: ServiceGraphNode[]; edges: ServiceGraphEdge[] }) {
  const navigate = useNavigate()
  const [focus, setFocus] = useState<GraphFocus>(null)
  const model = graphFocusModel(nodes, edges, focus)
  const dim = (on: boolean) => (model.active && !on ? 0.3 : 1)
  const activate = (url: string) => (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      navigate(url)
    }
  }
  const cols = Math.min(nodes.length, 4)
  const rows = Math.ceil(nodes.length / cols)
  const pos = new Map<string, { col: number; row: number }>()
  nodes.forEach((n, i) => pos.set(n.id, { col: i % cols, row: Math.floor(i / cols) }))

  const svgWidth = PAD * 2 + (cols - 1) * COL_GAP + NODE_W
  const svgHeight = PAD * 2 + (rows - 1) * ROW_GAP + NODE_H
  const center = (id: string) => {
    const p = pos.get(id)
    return p ? { x: PAD + p.col * COL_GAP + NODE_W / 2, y: PAD + p.row * ROW_GAP + NODE_H / 2 } : null
  }

  const nodeById = new Map(nodes.map((n) => [n.id, n]))
  const nodeName = (id: string) => nodeById.get(id)?.label ?? id
  const twoWay = twoWayEdges(edges)
  const counts = { warning: 0, critical: 0 }
  for (const n of nodes) {
    const h = healthKind(n.health)
    if (h === 'warning' || h === 'critical') counts[h] += 1
  }
  const summary = `Service dependency graph: ${nodes.length} service${nodes.length === 1 ? '' : 's'}, ${edges.length} connection${edges.length === 1 ? '' : 's'}; ${counts.warning} warning, ${counts.critical} critical. A table listing every service and connection follows.`

  return (
    <div>
      <div className="table-wrap">
        <svg width={svgWidth} height={svgHeight} role="group" aria-label={summary}>
          <defs>
            {Object.values(EDGE_STYLE).map((s) => (
              <marker key={s.marker} id={s.marker} markerWidth="8" markerHeight="6" refX="8" refY="3" orient="auto">
                <polygon points="0 0, 8 3, 0 6" fill={s.color} />
              </marker>
            ))}
          </defs>

          {edges.map((edge, i) => {
            const from = center(edge.source)
            const to = center(edge.target)
            if (!from || !to || edge.source === edge.target) return null
            const style = EDGE_STYLE[verdictKind(edge.verdict)]
            const { start, end, mid } = edgeGeometry(from, to, NODE_W, NODE_H, twoWay[i] ? EDGE_OFFSET : 0)
            const lat = latencyLabel(edge.latency)
            const active = model.edgeIndexes.has(i)
            const fromName = nodeName(edge.source)
            const toName = nodeName(edge.target)
            const url = flowsUrlForEdge(fromName, toName)
            return (
              <g
                key={`edge-${i}`}
                role="link"
                tabIndex={0}
                aria-label={`Connection ${fromName} to ${toName}, ${edge.verdict || 'unknown verdict'}${lat ? `, ${lat}` : ''}. Open matching flows.`}
                opacity={dim(active)}
                onClick={() => navigate(url)}
                onKeyDown={activate(url)}
                onMouseEnter={() => setFocus({ kind: 'edge', index: i })}
                onMouseLeave={() => setFocus(null)}
                onFocus={() => setFocus({ kind: 'edge', index: i })}
                onBlur={() => setFocus(null)}
              >
                <line x1={start.x} y1={start.y} x2={end.x} y2={end.y} stroke="transparent" strokeWidth="14" />
                <line x1={start.x} y1={start.y} x2={end.x} y2={end.y} stroke={style.color} strokeWidth={active ? 3 : 1.5} strokeDasharray={style.dash} markerEnd={`url(#${style.marker})`} />
                {lat && (
                  <text x={mid.x} y={mid.y - 5} textAnchor="middle" fontSize="9" fill="var(--text-tertiary)" fontFamily="monospace">
                    {lat}
                  </text>
                )}
              </g>
            )
          })}

          {nodes.map((node) => {
            const p = pos.get(node.id)!
            const x = PAD + p.col * COL_GAP
            const y = PAD + p.row * ROW_GAP
            const health = healthKind(node.health)
            const active = focus?.kind === 'node' && focus.id === node.id
            const url = flowsUrlForService(node.label)
            return (
              <g
                key={node.id}
                role="link"
                tabIndex={0}
                aria-label={`Service ${node.label}, ${HEALTH_LABEL[health]}, ${node.flowCount} connections. Open its flows.`}
                opacity={dim(model.nodeIds.has(node.id))}
                onClick={() => navigate(url)}
                onKeyDown={activate(url)}
                onMouseEnter={() => setFocus({ kind: 'node', id: node.id })}
                onMouseLeave={() => setFocus(null)}
                onFocus={() => setFocus({ kind: 'node', id: node.id })}
                onBlur={() => setFocus(null)}
              >
                <rect x={x} y={y} width={NODE_W} height={NODE_H} rx="10" ry="10" fill="var(--bg-elevated-2)" stroke={HEALTH_STROKE[health]} strokeWidth={active ? 3 : 1.5} strokeDasharray={health === 'unknown' ? '4 3' : undefined} />
                <text x={x + NODE_W / 2} y={y + NODE_H / 2 - 4} textAnchor="middle" fontSize="12" fontWeight="600" fill={health === 'critical' ? 'var(--danger)' : 'var(--text-primary)'}>
                  {node.label}
                </text>
                <text x={x + NODE_W / 2} y={y + NODE_H / 2 + 12} textAnchor="middle" fontSize="9" fill="var(--text-tertiary)">
                  {node.flowCount} connections
                </text>
              </g>
            )
          })}
        </svg>
      </div>

      <div className="list-row" role="status" aria-live="polite" aria-atomic="true">
        {model.readout ? (
          <div className="grow">
            <b>{model.readout.heading}</b>
            <small>{model.readout.rows.map((r) => `${r.label}: ${r.value}`).join(' · ')}</small>
          </div>
        ) : (
          <small className="faint grow">Hover or focus a service or connection to see details. Press Enter to open its flows.</small>
        )}
      </div>

      <div className="row faint mt-10">
        <span>Node border:</span>
        {(Object.keys(HEALTH_STROKE) as HealthKind[]).map((h) => (
          <span key={h} className="row">
            <svg width="16" height="12" aria-hidden="true">
              <rect x="1.5" y="1.5" width="13" height="9" rx="3" fill="none" stroke={HEALTH_STROKE[h]} strokeWidth="2" strokeDasharray={h === 'unknown' ? '3 2' : undefined} />
            </svg>
            {HEALTH_LABEL[h]}
          </span>
        ))}
      </div>
      <div className="row faint mt-10">
        <span>Edge:</span>
        {(Object.keys(EDGE_STYLE) as VerdictKind[]).map((k) => (
          <span key={k} className="row">
            <svg width="20" height="8" aria-hidden="true">
              <line x1="0" y1="4" x2="20" y2="4" stroke={EDGE_STYLE[k].color} strokeWidth="2" strokeDasharray={EDGE_STYLE[k].dash} />
            </svg>
            {EDGE_STYLE[k].label}
          </span>
        ))}
      </div>

      <div className="sr-only">
        <table>
          <TableCaption>Services in the dependency graph</TableCaption>
          <thead>
            <tr>
              <th scope="col">Service</th>
              <th scope="col">Health</th>
              <th scope="col">Connections</th>
            </tr>
          </thead>
          <tbody>
            {nodes.map((n) => (
              <tr key={n.id}>
                <td>{n.label}</td>
                <td>{HEALTH_LABEL[healthKind(n.health)]}</td>
                <td className="num">{n.flowCount}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <table>
          <TableCaption>Connections in the dependency graph</TableCaption>
          <thead>
            <tr>
              <th scope="col">Connection</th>
              <th scope="col">Verdict</th>
              <th scope="col">Latency</th>
            </tr>
          </thead>
          <tbody>
            {edges.map((e, i) => (
              <tr key={i}>
                <td>
                  {nodeName(e.source)} to {nodeName(e.target)}
                </td>
                <td>{e.verdict || 'Unknown'}</td>
                <td>{latencyLabel(e.latency) ?? 'n/a'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function SeverityBadge({ severity }: { severity: string }) {
  return <span className={`pill ${severityTone(severity)}`}>{severity}</span>
}
