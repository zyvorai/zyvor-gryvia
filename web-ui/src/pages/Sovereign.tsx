// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { formatNumber } from '@/lib/format'
import { product, stateTone, zyntraLink, zyntraSummary, type SovereignProduct } from '@/lib/sovereign'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import PageHero from '@/components/PageHero'
import { ErrorState, Skeleton } from '@/components/StateViews'

function StatePill({ p }: { p: SovereignProduct }) {
  return (
    <span className={`pill ${stateTone(p.state)}`} title={p.error}>
      {p.state}
    </span>
  )
}

function ExternalLink({ href, children }: { href?: string; children: string }) {
  if (!href) return null
  return (
    <a href={href} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  )
}

function NotInstalled({ name, value }: { name: string; value: string }) {
  return (
    <p className="muted">
      {name} is not wired to this console. The Sovereign AI OS chart (helm/sovereign-aios) sets <span className="mono">{value}</span>.
    </p>
  )
}

function ZyntraCard({ p }: { p?: SovereignProduct }) {
  return (
    <section className="card" aria-labelledby="sov-zyntra">
      <p className="eyebrow">Ontology and decisions</p>
      <h2 className="card-title" id="sov-zyntra">
        Zyntra {p && <StatePill p={p} />}
      </h2>
      <p>Gryvia's objects as a typed ontology, with KPIs, typed actions, quorum approvals and a signed audit trail.</p>
      {!p?.installed ? (
        <NotInstalled name="Zyntra" value="apiGateway.sovereign.zyntraURL" />
      ) : p.state !== 'healthy' ? (
        <p className="text-bad">{p.error ?? `Zyntra answered ${p.state}.`}</p>
      ) : (
        <>
          <p className="muted">{zyntraSummary(p) || 'Connected.'}</p>
          <div className="table-wrap">
            <table>
              <caption className="sr-only">Zyntra at a glance</caption>
              <tbody>
                <tr>
                  <th scope="row">Pack</th>
                  <td className="mono">{p.pack ?? '—'}</td>
                </tr>
                <tr>
                  <th scope="row">Open gaps</th>
                  <td>{p.openGaps === undefined ? '—' : formatNumber(p.openGaps)}</td>
                </tr>
                <tr>
                  <th scope="row">Proposals waiting for approval</th>
                  <td>{p.pendingProposals === undefined ? '—' : formatNumber(p.pendingProposals)}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </>
      )}
      <div className="toolbar mt-10">
        <ExternalLink href={zyntraLink(p?.console, 'approvals')}>Approvals</ExternalLink>
        <ExternalLink href={zyntraLink(p?.console, 'gaps')}>Gaps</ExternalLink>
        <ExternalLink href={zyntraLink(p?.console, 'audit')}>Audit</ExternalLink>
        {p?.installed && !p.console && <small className="faint">Set apiGateway.sovereign.zyntraConsoleURL for links.</small>}
      </div>
    </section>
  )
}

function NetraCard({ p }: { p?: SovereignProduct }) {
  return (
    <section className="card" aria-labelledby="sov-netra">
      <p className="eyebrow">Network</p>
      <h2 className="card-title" id="sov-netra">
        Netra {p && <StatePill p={p} />}
      </h2>
      <p>eBPF flows, policies and detections for every pod, including the GPU fabric's east-west traffic.</p>
      {!p?.installed ? (
        <NotInstalled name="Netra" value="apiGateway.netra.url" />
      ) : (
        p.state !== 'healthy' && <p className="text-bad">{p.error ?? `Netra answered ${p.state}.`}</p>
      )}
      <div className="toolbar mt-10">
        {p?.installed && <Link to="/network">Network in this console</Link>}
        <ExternalLink href={p?.console ?? undefined}>Open Netra</ExternalLink>
      </div>
    </section>
  )
}

function GryviaCard() {
  const stats = useQuery({ queryKey: ['clusterStats'], queryFn: api.getClusterStats, refetchInterval: 30000 })
  const s = stats.data
  return (
    <section className="card" aria-labelledby="sov-gryvia">
      <p className="eyebrow">Compute and models</p>
      <h2 className="card-title" id="sov-gryvia">
        Gryvia <span className="pill ok">this console</span>
      </h2>
      <p>GPU scheduling, training, serving, the metered LLM gateway and agents that call your tools.</p>
      {s && (
        <p className="muted">
          {formatNumber(s.availableGPUs)} of {formatNumber(s.totalGPUs)} GPUs free · {formatNumber(s.runningJobs)} jobs running · {formatNumber(s.pendingJobs)} queued
        </p>
      )}
      <div className="toolbar mt-10">
        <Link to="/agents">Agents</Link>
        <Link to="/llm">LLM gateway</Link>
        <Link to="/nodes">Nodes</Link>
      </div>
    </section>
  )
}

const LOOP: [string, string][] = [
  ['Read', 'Zyntra ingests GPU nodes, jobs, tenants and inference services from Gryvia and flows from Netra, with provenance on every property.'],
  ['Propose', 'Agents search the ontology with a viewer and proposer token and draft typed actions. They cannot approve.'],
  ['Approve', 'People approve in Zyntra under quorum and maintenance windows; every step is hash-chained in the audit trail.'],
  ['Apply', 'Zyntra applies the approved change to Gryvia through the Kubernetes API: priorities, GPU sharing, job suspend.'],
]

export default function Sovereign() {
  useDocumentTitle('Sovereign AI OS')
  const q = useQuery({ queryKey: ['sovereign'], queryFn: api.getSovereign, refetchInterval: 30000 })
  const hero = (
    <PageHero
      eyebrow="Sovereign AI OS"
      title="Your models, your data, your decisions."
      lede="Gryvia runs the compute, Zyntra turns it into an ontology people decide on, and Netra watches the network. All three run in your cluster."
    />
  )
  if (q.isLoading) {
    return (
      <>
        {hero}
        <Skeleton rows={4} />
      </>
    )
  }
  if (q.isError && !q.data) {
    return (
      <>
        {hero}
        <ErrorState title="Could not load the Sovereign AI OS status." error={q.error} onRetry={() => q.refetch()} retrying={q.isRefetching} />
      </>
    )
  }
  return (
    <>
      {hero}
      <div className="grid">
        <GryviaCard />
        <ZyntraCard p={product(q.data, 'Zyntra')} />
        <NetraCard p={product(q.data, 'Netra')} />
        <section className="card span3" aria-labelledby="sov-loop">
          <p className="eyebrow">How a change happens</p>
          <h2 className="card-title" id="sov-loop">
            Read, propose, approve, apply.
          </h2>
          <ol className="stack">
            {LOOP.map(([step, text]) => (
              <li key={step}>
                <strong>{step}.</strong> {text}
              </li>
            ))}
          </ol>
        </section>
      </div>
    </>
  )
}
