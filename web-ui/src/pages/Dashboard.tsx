// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '@/lib/api'
import type { GryviaAIJob } from '@/types'
import GPUChart from '@/components/GPUChart'
import PageHero from '@/components/PageHero'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { countByGroup, phaseTone } from '@/lib/phase'
import { jobFramework, jobGpus, jobStatusGroup } from '@/lib/jobs'
import { SrOnly } from '@/components/TableCaption'
import { formatPercent } from '@/lib/format'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'

export default function Dashboard() {
  useDocumentTitle('Dashboard')
  const stats = useQuery({ queryKey: ['clusterStats'], queryFn: api.getClusterStats, refetchInterval: 15000 })
  const jobsQ = useQuery({ queryKey: ['jobs'], queryFn: api.getJobs, refetchInterval: 10000 })
  const nodesQ = useQuery({ queryKey: ['nodes'], queryFn: api.getNodes, refetchInterval: 30000 })
  const gpuQ = useQuery({ queryKey: ['gpuMetrics'], queryFn: api.getGPUMetrics, refetchInterval: 15000 })

  const clusterStats = stats.data
  const jobs = jobsQ.data
  const nodes = nodesQ.data
  const gpuMetrics = gpuQ.data

  if (stats.isError && !clusterStats) {
    return (
      <>
        <PageHero eyebrow="Dashboard" title="Cluster unavailable." tint="red" />
        <ErrorState title="Could not load cluster stats." error={stats.error} onRetry={() => stats.refetch()} retrying={stats.isRefetching} />
      </>
    )
  }

  const counts = countByGroup(jobs, (j) => j.status?.phase)
  const finished = counts.completed + counts.failed
  const successRate = finished > 0 ? Math.round((counts.completed / finished) * 100) : undefined
  const utilization = clusterStats ? Math.round(clusterStats.utilizationPercent || 0) : undefined
  const totalGPUs = clusterStats?.totalGPUs ?? 0
  const availableGPUs = clusterStats?.availableGPUs ?? 0
  const allocatedGPUs = clusterStats?.allocatedGPUs ?? Math.max(0, totalGPUs - availableGPUs)
  const num = (n: number | undefined) => (n === undefined ? '—' : n)

  return (
    <>
      <PageHero eyebrow="Dashboard" title="Your GPU cluster, at a glance." lede="Live GPU capacity, job status and node health." />

      <div className="grid">
        {stats.isError && (
          <div className="span3">
            <ErrorState title="Could not refresh cluster stats; showing the last data." error={stats.error} onRetry={() => stats.refetch()} retrying={stats.isRefetching} />
          </div>
        )}

        <div className="apple-metric-band span3">
          <div>
            <span>GPUs{clusterStats ? ` · ${availableGPUs} available` : ''}</span>
            <b>{num(clusterStats?.totalGPUs)}</b>
          </div>
          <div>
            <span>
              <Link to="/jobs?f_status=Running" className="card-link">
                Running jobs
              </Link>
              {jobs && counts.pending > 0 && (
                <>
                  {' · '}
                  <Link to="/jobs?f_status=Pending" className="card-link">
                    {counts.pending} pending
                  </Link>
                </>
              )}
            </span>
            <b>{num(jobs ? counts.running : undefined)}</b>
          </div>
          <div>
            <span>
              <Link to="/jobs?f_status=Completed" className="card-link">
                Completed
              </Link>
              {successRate !== undefined ? ` · ${formatPercent(successRate)} success` : ''}
            </span>
            <b>{num(jobs ? counts.completed : undefined)}</b>
          </div>
          <div>
            <span>GPU utilization now</span>
            <b>{formatPercent(utilization)}</b>
          </div>
        </div>

        <section className="card span2">
          <p className="eyebrow">GPUS</p>
          <h2 className="card-title">Utilization by GPU</h2>
          {gpuQ.isError && !gpuMetrics ? (
            <ErrorState title="Could not load GPU metrics." error={gpuQ.error} onRetry={() => gpuQ.refetch()} retrying={gpuQ.isRefetching} />
          ) : gpuQ.isLoading ? (
            <Skeleton rows={4} />
          ) : !gpuMetrics?.metrics || gpuMetrics.metrics.length === 0 ? (
            <EmptyState title="No GPU samples yet">GPU metrics are reported by the GPU collector on each node. Check that the GPU operator and its collector pods are running.</EmptyState>
          ) : (
            <GPUChart
              data={gpuMetrics.metrics.map((m) => ({
                time: new Date(m.timestamp).toLocaleTimeString(),
                [`${m.node}-GPU${m.gpuIndex}`]: m.utilization,
              }))}
            />
          )}
        </section>

        <section className="card">
          <p className="eyebrow">STATUS</p>
          <h2 className="card-title">Cluster health</h2>
          {!clusterStats ? (
            <Skeleton rows={5} />
          ) : (
            <>
              <HealthCheck label="GPU nodes" tone={clusterStats.totalNodes > 0 ? 'ok' : 'warn'} detail={`${clusterStats.totalNodes} nodes`} />
              <HealthCheck label="GPU utilization" tone={(utilization ?? 0) >= 95 ? 'bad' : (utilization ?? 0) > 80 ? 'warn' : 'ok'} detail={formatPercent(utilization)} />
              <HealthCheck label="Failed jobs" tone={!jobs ? undefined : counts.failed === 0 ? 'ok' : 'warn'} detail={jobs ? `${counts.failed}` : '—'} to="/jobs?f_status=Failed" />
              <HealthCheck
                label="GPUs allocated"
                tone={totalGPUs > 0 && availableGPUs === 0 ? 'warn' : 'ok'}
                detail={`${allocatedGPUs}/${totalGPUs} GPUs`}
              />
              <HealthCheck
                label="Job queue"
                tone={!jobs ? undefined : counts.pending >= 10 ? 'bad' : counts.pending > 5 ? 'warn' : 'ok'}
                detail={jobs ? `${counts.pending} pending` : '—'}
                to="/jobs?f_status=Pending"
              />
            </>
          )}
        </section>

        <section className="card span2">
          <p className="eyebrow">JOBS · {jobs ? `${jobs.length} TOTAL` : '…'}</p>
          <h2 className="card-title">Recent jobs</h2>
          {jobsQ.isError && !jobs ? (
            <ErrorState title="Could not load jobs." error={jobsQ.error} onRetry={() => jobsQ.refetch()} retrying={jobsQ.isRefetching} />
          ) : jobsQ.isLoading ? (
            <Skeleton rows={4} />
          ) : (jobs || []).length === 0 ? (
            <EmptyState
              title="No jobs yet"
              action={
                <Link to="/jobs/new" className="buttonlike primary">
                  Submit a job
                </Link>
              }
            >
              Jobs are GryviaAIJob resources; the Gryvia operator schedules them onto GPU nodes once you submit one.
            </EmptyState>
          ) : (
            (jobs || []).slice(0, 5).map((job) => <JobRow key={job.metadata?.name} job={job} />)
          )}
          <p>
            <Link to="/jobs" className="card-link">
              View all jobs ›
            </Link>
          </p>
        </section>

        <section className="card">
          <p className="eyebrow">CAPACITY</p>
          <h2 className="card-title">GPU nodes</h2>
          {nodesQ.isError && !nodes ? (
            <ErrorState title="Could not load nodes." error={nodesQ.error} onRetry={() => nodesQ.refetch()} retrying={nodesQ.isRefetching} />
          ) : nodesQ.isLoading ? (
            <Skeleton rows={3} />
          ) : !nodes || nodes.length === 0 ? (
            <EmptyState title="No GPU nodes registered">GryviaGpuNode resources are registered by the GPU operator once it discovers GPUs. Check that the operator is running.</EmptyState>
          ) : (
            <>
              {nodes.slice(0, 6).map((node) => (
                <Link key={node.metadata?.name} to={`/nodes?q=${encodeURIComponent(node.spec?.nodeName || node.metadata?.name || '')}`} className="list-row">
                  <span className={`dot ${phaseTone(node.status?.phase)}`} aria-hidden="true" />
                  <div className="grow">
                    <b>{node.spec?.nodeName || node.metadata?.name}</b>
                    <small>
                      {node.spec?.gpuType} × {node.spec?.gpuCount}
                    </small>
                  </div>
                  <span className="faint">{node.status?.phase || 'Unknown'}</span>
                </Link>
              ))}
              {nodes.length > 6 && (
                <p>
                  <Link to="/nodes" className="card-link">
                    View all {nodes.length} nodes ›
                  </Link>
                </p>
              )}
            </>
          )}
        </section>

        <section className="card span3">
          <p className="eyebrow">NEXT STEPS</p>
          <h2 className="card-title">Quick actions</h2>
          <div className="toolbar">
            <Link to="/jobs/new" className="buttonlike primary">
              Submit job
            </Link>
            <Link to="/quotas" className="buttonlike btn-secondary">
              Quotas
            </Link>
            <Link to="/costs" className="buttonlike btn-secondary">
              Costs
            </Link>
          </div>
        </section>
      </div>
    </>
  )
}

function JobRow({ job }: { job: GryviaAIJob }) {
  const phase = job.status?.phase || 'Unknown'
  return (
    <div className="list-row">
      <Link to={`/jobs/${job.metadata?.name}`} className="grow link-plain">
        <b className="mono">{job.metadata?.name}</b>
        <small>
          {jobFramework(job)} · {jobGpus(job)}
        </small>
      </Link>
      <Link to={`/jobs?f_status=${encodeURIComponent(jobStatusGroup(job))}`} className={`pill ${phaseTone(phase)} link-plain`} title={`Show ${phase.toLowerCase()} jobs`}>
        {phase}
      </Link>
    </div>
  )
}

const TONE_LABEL = { ok: 'Healthy', warn: 'Warning', bad: 'Critical' } as const

function HealthCheck({ label, tone, detail, to }: { label: string; tone?: 'ok' | 'warn' | 'bad'; detail?: string; to?: string }) {
  const body = (
    <>
      <span className={`dot ${tone ?? ''}`} aria-hidden="true" />
      <SrOnly>{tone ? `${TONE_LABEL[tone]}: ` : ''}</SrOnly>
      <span className="grow">{label}</span>
      {detail && <span className="faint num">{detail}</span>}
    </>
  )
  return to ? (
    <Link to={to} className="list-row">
      {body}
    </Link>
  ) : (
    <div className="list-row">{body}</div>
  )
}
