// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate } from 'react-router-dom'
import { api } from '@/lib/api'
import DataTable, { type Column } from '@/components/DataTable'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import { countTone } from '@/components/kit/tone'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'
import { useTableState } from '@/hooks/useTableState'
import { countByGroup, phaseTone } from '@/lib/phase'
import { errorMessage } from '@/lib/errors'
import { formatDate, formatRelative } from '@/lib/format'
import { JOB_LIST_LIMIT, STATUS_FILTER_OPTIONS, jobCreatedMs, jobFramework, jobSearchText, jobStatusGroup, jobTeam } from '@/lib/jobs'
import type { FilterDef, SortAccessor } from '@/lib/tableState'
import type { GryviaAIJob } from '@/types'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'

const FILTERS: FilterDef<GryviaAIJob>[] = [
  { name: 'status', label: 'Status', get: jobStatusGroup, options: STATUS_FILTER_OPTIONS },
  { name: 'team', label: 'Team', get: jobTeam },
  { name: 'framework', label: 'Framework', get: jobFramework },
]
const SORTS: Record<string, SortAccessor<GryviaAIJob>> = {
  name: (j) => j.metadata?.name,
  status: (j) => j.status?.phase || 'Unknown',
  gpus: (j) => j.spec?.gpus ?? 0,
  age: jobCreatedMs,
}
const DEFAULT_SORT = { key: 'age', dir: 'desc' as const }

const STATUS_BUTTONS = ['Running', 'Pending', 'Completed', 'Failed'] as const

export default function Jobs() {
  useDocumentTitle('Jobs')
  const navigate = useNavigate()
  const { data: jobs, isLoading, isError, error, refetch, isRefetching, dataUpdatedAt } = useQuery({
    queryKey: ['jobs'],
    queryFn: api.getJobs,
    refetchInterval: 15000,
  })

  const table = useTableState({ rows: jobs, searchText: jobSearchText, filters: FILTERS, sortAccessors: SORTS, defaultSort: DEFAULT_SORT })

  const counts = countByGroup(jobs, (j) => j.status?.phase)
  const failed = counts.failed
  const loadFailed = isError && !jobs
  const activeStatus = (table.filterValues.status ?? '').toLowerCase()
  const truncated = (jobs?.length ?? 0) >= JOB_LIST_LIMIT

  const columns: Column<GryviaAIJob>[] = [
    {
      key: 'name',
      header: 'Name',
      sortable: true,
      render: (job) => (
        <Link to={`/jobs/${job.metadata.name}`} className="card-link mono" onClick={(e) => e.stopPropagation()}>
          {job.metadata.name}
        </Link>
      ),
    },
    { key: 'framework', header: 'Framework', render: jobFramework },
    { key: 'team', header: 'Team', render: (job) => jobTeam(job) ?? <span className="faint">—</span> },
    { key: 'gpus', header: 'GPUs', sortable: true, numeric: true, render: (job) => `${job.spec?.gpus ?? 0} × ${job.spec?.gpuType || 'GPU'}` },
    {
      key: 'status',
      header: 'Status',
      sortable: true,
      render: (job) => <span className={`pill ${phaseTone(job.status?.phase)}`}>{job.status?.phase || 'Unknown'}</span>,
    },
    {
      key: 'age',
      header: 'Age',
      sortable: true,
      render: (job) => (
        <span className="faint" title={formatDate(job.metadata.creationTimestamp)}>
          {formatRelative(job.metadata.creationTimestamp)}
        </span>
      ),
    },
  ]

  return (
    <>
      <PageHero eyebrow="Jobs" title="Every job, in one place." lede="Training and inference jobs across every team." />

      <div className="grid">
        <PagePulse
          updatedAt={dataUpdatedAt}
          error={isError ? errorMessage(error) : undefined}
          headline={
            jobs
              ? failed > 0
                ? `${failed} job${failed === 1 ? '' : 's'} failed.`
                : `${jobs.length} job${jobs.length === 1 ? '' : 's'}, none failed.`
              : undefined
          }
          tone={jobs && failed > 0 ? 'warn' : undefined}
          figures={[
            { label: 'Running', value: jobs ? counts.running : undefined },
            { label: 'Pending', value: jobs ? counts.pending : undefined },
            { label: 'Completed', value: jobs ? counts.completed : undefined },
            { label: 'Failed', value: jobs ? failed : undefined, tone: jobs ? countTone(failed) : undefined },
          ]}
        />

        <section className="card span3">
          <p className="eyebrow">WORKLOADS · {jobs ? `${jobs.length} TOTAL` : '…'}</p>
          <h2 className="card-title">All jobs</h2>
          {jobs && (
            <div className="toolbar" role="group" aria-label="Filter by status">
              {STATUS_BUTTONS.map((s) => {
                const n = counts[s.toLowerCase() as keyof typeof counts]
                const on = activeStatus === s.toLowerCase()
                return (
                  <button key={s} type="button" className={on ? 'primary' : 'btn-secondary'} aria-pressed={on} onClick={() => table.setFilter('status', on ? '' : s)}>
                    {s} <span className="num">{n}</span>
                  </button>
                )
              })}
            </div>
          )}
          {isError && jobs && <ErrorState title="Could not refresh jobs; showing the last data." error={error} onRetry={() => refetch()} retrying={isRefetching} />}
          {truncated && (
            <p className="faint" role="note">
              The list is limited to {JOB_LIST_LIMIT} jobs; older jobs may not be shown.
            </p>
          )}
          {loadFailed ? (
            <ErrorState title="Could not load jobs." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          ) : isLoading ? (
            <Skeleton rows={5} />
          ) : (
            <DataTable
              caption="Jobs"
              columns={columns}
              state={table}
              rowKey={(j) => j.metadata.name}
              onRowClick={(j) => navigate(`/jobs/${j.metadata.name}`)}
              searchLabel="Search jobs"
              actions={
                <>
                  <Link to="/jobs/new" className="buttonlike primary">
                    Submit job
                  </Link>
                  <button type="button" className="btn-refresh" onClick={() => refetch()} disabled={isRefetching} aria-busy={isRefetching}>
                    {isRefetching ? 'Refreshing…' : 'Refresh'}
                  </button>
                </>
              }
              empty={
                <EmptyState
                  title="No jobs yet"
                  action={
                    <Link to="/jobs/new" className="buttonlike primary">
                      Submit a job
                    </Link>
                  }
                >
                  Jobs are GryviaAIJob resources, created from this UI or with kubectl. The Gryvia operator schedules them onto GPU nodes.
                </EmptyState>
              }
            />
          )}
        </section>
      </div>
    </>
  )
}
