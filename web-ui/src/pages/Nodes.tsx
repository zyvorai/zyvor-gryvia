// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '@/lib/api'
import DataTable, { type Column } from '@/components/DataTable'
import { useTableState } from '@/hooks/useTableState'
import type { FilterDef, SortAccessor } from '@/lib/tableState'
import type { GryviaGpuNode } from '@/types'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import { countTone } from '@/components/kit/tone'
import Progress from '@/components/Progress'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { formatBytes, formatPercent } from '@/lib/format'
import { phaseTone } from '@/lib/phase'
import { errorMessage } from '@/lib/errors'
import { notify } from '@/lib/notify'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'

const nodeName = (n: GryviaGpuNode) => n.spec?.nodeName || n.metadata?.name
const FILTERS: FilterDef<GryviaGpuNode>[] = [
  { name: 'phase', label: 'Phase', get: (n) => n.status?.phase || 'Unknown' },
  { name: 'gpu', label: 'GPU type', get: (n) => n.spec?.gpuType },
]
const SORTS: Record<string, SortAccessor<GryviaGpuNode>> = {
  name: nodeName,
  phase: (n) => n.status?.phase || 'Unknown',
  gpus: (n) => n.spec?.gpuCount ?? 0,
}
const searchText = (n: GryviaGpuNode) => [nodeName(n), n.spec?.gpuType, n.status?.phase, n.spec?.rdma ? 'rdma' : ''].filter(Boolean).join(' ')
const jobsLink = (n: GryviaGpuNode) => `/jobs?q=${encodeURIComponent(nodeName(n) ?? '')}`

const isNum = (n: unknown): n is number => typeof n === 'number' && Number.isFinite(n)

async function copyUuid(uuid: string) {
  try {
    await navigator.clipboard.writeText(uuid)
    notify.success('GPU UUID copied')
  } catch (err) {
    notify.error('Could not copy the UUID', err)
  }
}

export default function Nodes() {
  useDocumentTitle('Nodes')
  const { data: nodes, isLoading, isError, error, refetch, isRefetching, dataUpdatedAt } = useQuery({
    queryKey: ['nodes'],
    queryFn: api.getNodes,
    refetchInterval: 10000,
  })

  const table = useTableState({ rows: nodes, searchText, filters: FILTERS, sortAccessors: SORTS, defaultSort: { key: 'name', dir: 'asc' }, pageSize: 10 })

  if (isLoading) {
    return (
      <>
        <PageHero eyebrow="Nodes" title="Every GPU, accounted for." lede="GPU nodes, their GPUs and live health metrics." />
        <Skeleton rows={4} />
      </>
    )
  }

  if (isError && !nodes) {
    return (
      <>
        <PageHero eyebrow="Nodes" title="Nodes unavailable." tint="red" />
        <ErrorState title="Could not load GPU nodes." error={error} onRetry={() => refetch()} retrying={isRefetching} />
      </>
    )
  }

  const list = nodes || []
  const columns: Column<GryviaGpuNode>[] = [
    { key: 'name', header: 'Node', sortable: true, render: (n) => <span className="mono">{nodeName(n)}</span> },
    { key: 'phase', header: 'Phase', sortable: true, render: (n) => <span className={`pill ${phaseTone(n.status?.phase)}`}>{n.status?.phase || 'Unknown'}</span> },
    { key: 'gpu', header: 'GPU type', render: (n) => n.spec?.gpuType || '—' },
    { key: 'gpus', header: 'GPUs', sortable: true, numeric: true, render: (n) => n.spec?.gpuCount ?? '—' },
    {
      key: 'jobs',
      header: 'Jobs',
      render: (n) => (
        <Link to={jobsLink(n)} className="card-link">
          Jobs on this node
        </Link>
      ),
    },
  ]
  const ready = list.filter((n) => phaseTone(n.status?.phase) === 'ok').length
  const notReady = list.length - ready

  return (
    <>
      <PageHero eyebrow="Nodes" title="Every GPU, accounted for." lede="GPU nodes, their GPUs and live health metrics." />

      <div className="grid">
        {isError && (
          <div className="span3">
            <ErrorState title="Could not refresh nodes; showing the last data." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}

        <PagePulse
          updatedAt={dataUpdatedAt}
          error={isError ? errorMessage(error) : undefined}
          headline={
            list.length === 0
              ? 'No GPU nodes registered.'
              : notReady > 0
                ? `${notReady} node${notReady === 1 ? '' : 's'} not ready.`
                : `All ${list.length} nodes ready.`
          }
          tone={list.length === 0 ? undefined : notReady > 0 ? 'warn' : 'ok'}
          figures={[
            { label: 'Total nodes', value: list.length },
            { label: 'Total GPUs', value: list.reduce((sum, n) => sum + (n.spec?.gpuCount || 0), 0) },
            { label: 'RDMA enabled', value: list.filter((n) => n.spec?.rdma).length },
            { label: 'Ready', value: ready },
            { label: 'Not ready', value: notReady, tone: countTone(notReady) },
          ]}
        />

        <section className="card span3">
          <p className="eyebrow">INVENTORY</p>
          <h2 className="card-title">All nodes</h2>
          <DataTable
            caption="GPU nodes"
            columns={columns}
            state={table}
            rowKey={(n) => n.metadata?.name}
            searchLabel="Search nodes"
            empty={
              <EmptyState title="No GPU nodes registered">
                GryviaGpuNode resources are created by the Gryvia GPU operator when it discovers GPUs on a node, and live metrics come from its GPU collector. Check that the operator is installed and its pods are running.
              </EmptyState>
            }
          />
        </section>

        {table.pageRows.map((node) => {
          const phase = node.status?.phase
          return (
            <section key={node.metadata?.name} className="card span3">
              <p className="eyebrow">NODE</p>
              <h2 className="card-title mono">{node.spec?.nodeName || node.metadata?.name}</h2>
              <div className="row">
                <span className={`pill ${phaseTone(phase)}`}>{phase || 'Unknown'}</span>
                {node.spec?.rdma && <span className="pill info">RDMA</span>}
                <Link to={jobsLink(node)} className="card-link">
                  Jobs on this node ›
                </Link>
              </div>

              <div className="apple-metric-band">
                <div>
                  <span>GPU type</span>
                  <b>{node.spec?.gpuType || '—'}</b>
                </div>
                <div>
                  <span>GPU count</span>
                  <b>{node.spec?.gpuCount ?? '—'}</b>
                </div>
                <div>
                  <span>GPU memory (per GPU)</span>
                  <b>{node.spec?.memoryGB ? formatBytes(node.spec.memoryGB * 1024 ** 3) : '—'}</b>
                </div>
              </div>

              {node.status?.gpuStatus && node.status.gpuStatus.length > 0 && (
                <div className="formgrid">
                  {node.status.gpuStatus.map((gpu) => {
                    const hasMem = isNum(gpu.memoryUsed) && isNum(gpu.memoryTotal) && gpu.memoryTotal > 0
                    const memUsed = hasMem ? (gpu.memoryUsed as number) * 1024 ** 2 : 0
                    const memTotal = hasMem ? (gpu.memoryTotal as number) * 1024 ** 2 : 0
                    const memPercent = hasMem ? Math.min(100, (memUsed / memTotal) * 100) : 0
                    const gpuName = `${node.spec?.nodeName || node.metadata?.name} GPU ${gpu.index}`
                    return (
                      <div key={gpu.index} className="stack">
                        <div className="row">
                          <b>GPU {gpu.index}</b>
                          {gpu.health && <span className={`pill ${phaseTone(gpu.health)}`}>{gpu.health}</span>}
                          {gpu.uuid && (
                            <button type="button" className="btn-secondary mono" title={gpu.uuid} aria-label={`Copy UUID of GPU ${gpu.index}`} onClick={() => copyUuid(gpu.uuid!)}>
                              {gpu.uuid.length > 16 ? `${gpu.uuid.slice(0, 12)}…` : gpu.uuid}
                            </button>
                          )}
                        </div>
                        <MetricBar
                          label="Temperature"
                          available={isNum(gpu.temperature)}
                          value={`${gpu.temperature}°C`}
                          percent={Math.min(gpu.temperature ?? 0, 100)}
                          tone={(gpu.temperature ?? 0) > 80 ? 'bad' : (gpu.temperature ?? 0) > 70 ? 'warn' : undefined}
                          barLabel={`${gpuName} temperature`}
                        />
                        <MetricBar
                          label="Utilization"
                          available={isNum(gpu.utilization)}
                          value={formatPercent(gpu.utilization)}
                          percent={Math.min(gpu.utilization ?? 0, 100)}
                          barLabel={`${gpuName} utilization`}
                        />
                        <MetricBar
                          label="GPU memory"
                          available={hasMem}
                          value={`${formatBytes(memUsed)} / ${formatBytes(memTotal)}`}
                          percent={memPercent}
                          barLabel={`${gpuName} memory`}
                        />
                      </div>
                    )
                  })}
                </div>
              )}
            </section>
          )
        })}
      </div>
    </>
  )
}

function MetricBar({ label, value, percent, tone, available, barLabel }: {
  label: string; value: string; percent: number; tone?: 'warn' | 'bad'; available: boolean; barLabel: string
}) {
  return (
    <div>
      <div className="row">
        <span className="faint">{label}</span>
        <span className="num">{available ? value : '—'}</span>
      </div>
      <Progress value={available ? percent : 0} tone={tone} label={available ? barLabel : `${barLabel} (no data)`} />
    </div>
  )
}
