import { useEffect, useRef, useState } from 'react'
import axios from 'axios'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useParams, useNavigate } from 'react-router-dom'
import type { UseQueryResult } from '@tanstack/react-query'
import { api, type JobEvent, type JobLogs, type JobPod } from '@/lib/api'
import FlightRecorder from '@/components/FlightRecorder'
import FlightDiagnosis from '@/components/FlightDiagnosis'
import InferencePanel from '@/components/InferencePanel'
import PageHero from '@/components/PageHero'
import PagePulse from '@/components/kit/PagePulse'
import ConfirmDialog from '@/components/ConfirmDialog'
import ElasticResize from '@/components/ElasticResize'
import CopyButton from '@/components/CopyButton'
import { TableCaption } from '@/components/TableCaption'
import { ErrorState, Skeleton } from '@/components/StateViews'
import { useNow } from '@/lib/useNow'
import { phaseTone } from '@/lib/phase'
import { durationBetween, formatDate, formatRelative } from '@/lib/format'
import { errorMessage } from '@/lib/errors'
import { notify } from '@/lib/notify'
import { envSource, isSensitiveEnv, jobConditions, jobFramework, jobTeam, logText, podNodes, type EnvEntry } from '@/lib/jobs'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'

const shortDate = (iso?: string) => (iso ? formatDate(iso) : 'Not started')

async function copyText(label: string, text: string | undefined) {
  if (!text) {
    notify.error(`No ${label} to copy`)
    return
  }
  try {
    await navigator.clipboard.writeText(text)
    notify.success(`Copied ${label}`)
  } catch (err) {
    notify.error(`Could not copy the ${label}`, err)
  }
}

export default function JobDetails() {
  const { name } = useParams<{ name: string }>()
  useDocumentTitle(name ? `Job ${name}` : 'Job')
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const now = useNow()
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [revealed, setRevealed] = useState<Set<number>>(new Set())

  const { data: job, isLoading, isError, error, refetch, isRefetching, dataUpdatedAt } = useQuery({
    queryKey: ['job', name],
    queryFn: () => api.getJob(name!),
    enabled: !!name,
    refetchInterval: 5000,
    retry: (count, err) => !(axios.isAxiosError(err) && err.response?.status === 404) && count < 2,
  })

  const podsQ = useQuery({
    queryKey: ['job-pods', name],
    queryFn: () => api.getJobPods(name!),
    enabled: !!name,
    refetchInterval: 10000,
  })

  const deleteMutation = useMutation({
    mutationFn: () => api.deleteJob(name!),
    onSuccess: () => {
      notify.success(`Deleted job ${name}`)
      queryClient.invalidateQueries({ queryKey: ['jobs'] })
      queryClient.removeQueries({ queryKey: ['job', name] })
      navigate('/jobs')
    },
    onError: (err) => {
      setConfirmDelete(false)
      notify.error(`Could not delete job ${name}`, err)
    },
  })

  if (isLoading) {
    return (
      <>
        <PageHero eyebrow="Job" title={name ?? 'Job'} />
        <Skeleton rows={5} />
      </>
    )
  }

  const notFound = axios.isAxiosError(error) && error.response?.status === 404
  if ((isError && !job) || (!job && !isLoading)) {
    if (notFound || !isError) {
      return (
        <>
          <PageHero eyebrow="Job" title="Job not found" lede={name ? `There is no job named ${name}. It may have been deleted.` : undefined} />
          <Link to="/jobs" className="buttonlike primary">
            Back to jobs
          </Link>
        </>
      )
    }
    return (
      <>
        <PageHero eyebrow="Job" title="Job unavailable" tint="red" />
        <ErrorState title="Could not load this job." error={error} onRetry={() => refetch()} retrying={isRefetching} />
        <p>
          <Link to="/jobs" className="card-link">
            Back to jobs
          </Link>
        </p>
      </>
    )
  }
  if (!job) return null

  const spec = job.spec
  const phase = job.status?.phase || 'Unknown'
  const command = spec?.command?.join(' ')
  const env = (spec?.env ?? []) as EnvEntry[]
  const labels = Object.entries(job.metadata?.labels ?? {})
  const created = job.metadata?.creationTimestamp
  const team = jobTeam(job)
  const nodeNames = podNodes(podsQ.data ?? [])

  const toggleReveal = (idx: number) =>
    setRevealed((prev) => {
      const next = new Set(prev)
      if (next.has(idx)) next.delete(idx)
      else next.add(idx)
      return next
    })

  return (
    <>
      <PageHero
        eyebrow="Job"
        title={job.metadata.name}
        lede={job.status?.message || `${phase}${created ? ` · created ${formatRelative(created, now)}` : ''}`}
      />

      <div className="grid">
        <PagePulse
          updatedAt={dataUpdatedAt}
          error={isError ? errorMessage(error) : undefined}
          headline={`${job.metadata.name} is ${phase.toLowerCase()}.`}
          tone={phaseTone(phase) === 'bad' ? 'bad' : undefined}
          figures={[
            { label: 'Created', value: formatDate(created) },
            { label: 'Started', value: shortDate(job.status?.startTime) },
            {
              label: 'Duration',
              value: job.status?.startTime ? durationBetween(job.status.startTime, job.status.completionTime, now) : 'Not started',
            },
            { label: 'GPU type', value: spec?.gpuType || '—' },
            { label: 'GPU count', value: spec?.gpus },
            { label: 'Memory', value: spec?.resources?.requests?.memory ?? '—' },
            { label: 'CPU cores', value: spec?.resources?.requests?.cpu ?? '—' },
          ]}
        />

        {isError && (
          <div className="span3">
            <ErrorState title="Could not refresh this job; showing the last data." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        )}

        {job.metadata.namespace && <InferencePanel namespace={job.metadata.namespace} job={job.metadata.name} />}
        {job.metadata.namespace && <FlightRecorder namespace={job.metadata.namespace} job={job.metadata.name} />}
        {job.metadata.namespace && <FlightDiagnosis namespace={job.metadata.namespace} job={job.metadata.name} />}
        <ElasticResize job={job} />

        <section className="card span3">
          <p className="eyebrow">STATUS</p>
          <h2 className="card-title">Job actions</h2>
          <div className="toolbar">
            <Link to="/jobs" className="buttonlike btn-secondary">
              Back to jobs
            </Link>
            <span className={`pill ${phaseTone(phase)}`}>{phase}</span>
            <button type="button" className="btn-secondary" onClick={() => copyText('job name', job.metadata.name)}>
              Copy name
            </button>
            <button type="button" className="btn-secondary" disabled={!spec?.image} onClick={() => copyText('image', spec?.image)}>
              Copy image
            </button>
            <button type="button" className="btn-secondary" disabled={!command} onClick={() => copyText('command', command)}>
              Copy command
            </button>
            <button type="button" className="btn-secondary" onClick={() => navigate(`/jobs/new?clone=${encodeURIComponent(job.metadata.name)}`)}>
              Clone
            </button>
            <button type="button" className="danger" onClick={() => setConfirmDelete(true)}>
              Delete
            </button>
          </div>
        </section>

        <section className="card span2">
          <p className="eyebrow">SPEC</p>
          <h2 className="card-title">Job configuration</h2>
          <div className="formgrid">
            <div>
              <p className="faint">Framework</p>
              <p>{jobFramework(job)}</p>
            </div>
            <div>
              <p className="faint">Image</p>
              <p className="mono">{spec?.image || '—'}</p>
            </div>
            {team && (
              <div>
                <p className="faint">Team</p>
                <p>
                  <Link to={`/quotas?q=${encodeURIComponent(team)}`} className="card-link">
                    {team}
                  </Link>
                </p>
              </div>
            )}
            {nodeNames.length > 0 && (
              <div>
                <p className="faint">{nodeNames.length === 1 ? 'Node' : 'Nodes'}</p>
                <p>
                  {nodeNames.map((n, i) => (
                    <span key={n}>
                      {i > 0 && ', '}
                      <Link to={`/nodes?q=${encodeURIComponent(n)}`} className="card-link">
                        {n}
                      </Link>
                    </span>
                  ))}
                </p>
              </div>
            )}
            {spec?.distributed?.enabled && (
              <>
                <div>
                  <p className="faint">Distributed training</p>
                  <p>Enabled</p>
                </div>
                {spec.distributed.nodes && (
                  <div>
                    <p className="faint">Nodes</p>
                    <p>{spec.distributed.nodes}</p>
                  </div>
                )}
                {spec.distributed.gpusPerNode && (
                  <div>
                    <p className="faint">GPUs/node</p>
                    <p>{spec.distributed.gpusPerNode}</p>
                  </div>
                )}
              </>
            )}
          </div>
        </section>

        <section className="card">
          <p className="eyebrow">ENTRYPOINT</p>
          <h2 className="card-title">Command</h2>
          <code className="mono">{command || '—'}</code>
        </section>

        {env.length > 0 && (
          <section className="card span2">
            <p className="eyebrow">ENVIRONMENT</p>
            <h2 className="card-title">Environment variables</h2>
            {env.map((e, idx) => {
              const source = envSource(e)
              const sensitive = !source && isSensitiveEnv(e.name)
              const shown = revealed.has(idx)
              const literal = e.value ?? ''
              return (
                <div key={`${e.name}-${idx}`} className="list-row">
                  <span className="grow">{e.name}</span>
                  <span className="mono muted">
                    {source ?? (sensitive && !shown ? '********' : literal === '' ? '(empty)' : literal)}
                  </span>
                  {sensitive && (
                    <button type="button" className="btn-secondary" aria-label={`${shown ? 'Hide' : 'Reveal'} value of ${e.name}`} onClick={() => toggleReveal(idx)}>
                      {shown ? 'Hide' : 'Reveal'}
                    </button>
                  )}
                </div>
              )
            })}
          </section>
        )}

        <PodsCard pods={podsQ} />
        <LogsCard name={job.metadata.name} pods={podsQ} />
        <EventsCard name={job.metadata.name} />
        <ConditionsCard job={job} />

        {labels.length > 0 && (
          <section className="card">
            <p className="eyebrow">METADATA</p>
            <h2 className="card-title">Labels</h2>
            <div className="row">
              {labels.map(([key, value]) => (
                <span key={key} className="pill mono pill-raw">
                  {key}={value}
                </span>
              ))}
            </div>
          </section>
        )}
      </div>

      {confirmDelete && (
        <ConfirmDialog
          title={`Delete job ${job.metadata.name}?`}
          confirmLabel="Delete job"
          busy={deleteMutation.isPending}
          onCancel={() => setConfirmDelete(false)}
          onConfirm={() => deleteMutation.mutate()}
        >
          Job <b>{job.metadata.name}</b> and its pods will be removed. This cannot be undone.
        </ConfirmDialog>
      )}
    </>
  )
}

type PodsQuery = UseQueryResult<JobPod[], Error>

function podStateSummary(c: JobPod['containers'][number]): string {
  return `${c.name}: ${c.state}${c.reason ? ` (${c.reason})` : ''}${c.restartCount > 0 ? `, ${c.restartCount} restarts` : ''}`
}

function PodsCard({ pods }: { pods: PodsQuery }) {
  const list = pods.data
  return (
    <section className="card span3">
      <p className="eyebrow">RUNTIME</p>
      <h2 className="card-title">Pods</h2>
      {pods.isError && !list ? (
        <ErrorState title="Could not load pods." error={pods.error} onRetry={() => pods.refetch()} retrying={pods.isRefetching} />
      ) : pods.isLoading ? (
        <Skeleton rows={2} />
      ) : !list || list.length === 0 ? (
        <p className="faint">No pods yet. The Gryvia operator creates them once the job is scheduled onto a GPU node.</p>
      ) : (
        <div className="table-wrap">
          <table>
            <TableCaption>Pods of this job</TableCaption>
            <thead>
              <tr>
                <th scope="col">Pod</th>
                <th scope="col">Phase</th>
                <th scope="col">Node</th>
                <th scope="col" className="num">
                  Restarts
                </th>
                <th scope="col">Containers</th>
                <th scope="col">Message</th>
              </tr>
            </thead>
            <tbody>
              {list.map((p) => (
                <tr key={p.name}>
                  <td className="mono">{p.name}</td>
                  <td>
                    <span className={`pill ${phaseTone(p.phase)}`}>{p.phase || 'Unknown'}</span>
                  </td>
                  <td>
                    {p.node ? (
                      <Link to={`/nodes?q=${encodeURIComponent(p.node)}`} className="card-link">
                        {p.node}
                      </Link>
                    ) : (
                      <span className="faint">Not scheduled</span>
                    )}
                  </td>
                  <td className="num">{p.restarts}</td>
                  <td>
                    {p.containers.length === 0 ? (
                      <span className="faint">—</span>
                    ) : (
                      p.containers.map((c) => (
                        <div key={c.name} className={c.ready ? undefined : 'muted'}>
                          {podStateSummary(c)}
                        </div>
                      ))
                    )}
                  </td>
                  <td>{p.message || <span className="faint">—</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}

const TAILS = [100, 200, 500, 1000]

function downloadText(filename: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

function LogsCard({ name, pods }: { name: string; pods: PodsQuery }) {
  const now = useNow()
  const [podChoice, setPodChoice] = useState('')
  const [tail, setTail] = useState(200)
  const [follow, setFollow] = useState(false)
  const areaRef = useRef<HTMLPreElement>(null)

  const podList = pods.data ?? []
  const pod = podList.some((p) => p.name === podChoice) ? podChoice : podList[0]?.name
  const podInfo = podList.find((p) => p.name === pod)

  const logsQ = useQuery({
    queryKey: ['job-logs', name, pod, tail],
    queryFn: (): Promise<JobLogs & { message?: string }> => api.getJobLogs(name, { pod, tail }),
    enabled: Boolean(pod),
    // Poll only while following and the tab is visible; otherwise it is a one-off fetch (Refresh reloads).
    refetchInterval: () => (follow && document.visibilityState === 'visible' ? 3000 : false),
    retry: 1,
  })
  const logs = logsQ.data
  const lines = logs?.lines
  const text = logText(lines ?? [])

  useEffect(() => {
    const el = areaRef.current
    if (follow && el) el.scrollTop = el.scrollHeight
  }, [follow, lines])

  const notStarted = lines?.length === 0 && podInfo && podInfo.containers.length > 0 && podInfo.containers.every((c) => c.state.toLowerCase() === 'waiting')

  return (
    <section className="card span3">
      <p className="eyebrow">RUNTIME</p>
      <h2 className="card-title">Logs</h2>
      {pods.isLoading ? (
        <Skeleton rows={2} />
      ) : !pod ? (
        <p className="faint">No pods yet, so there are no logs to show.</p>
      ) : (
        <div className="stack">
          <div className="toolbar">
            {podList.length > 1 && (
              <label>
                Pod
                <select value={pod} onChange={(e) => setPodChoice(e.target.value)}>
                  {podList.map((p) => (
                    <option key={p.name} value={p.name}>
                      {p.name}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <label>
              Lines
              <select value={tail} onChange={(e) => setTail(Number(e.target.value))}>
                {TAILS.map((n) => (
                  <option key={n} value={n}>
                    Last {n}
                  </option>
                ))}
              </select>
            </label>
            <label className="row">
              <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
              <span>Follow</span>
            </label>
            <button type="button" className="btn-refresh" onClick={() => logsQ.refetch()} disabled={logsQ.isFetching} aria-busy={logsQ.isFetching}>
              {logsQ.isFetching ? 'Loading…' : 'Refresh'}
            </button>
            {logsQ.dataUpdatedAt > 0 && (
              <span className="faint" role="status">
                Updated {formatRelative(logsQ.dataUpdatedAt, now)}
              </span>
            )}
            <CopyButton value={text} label="logs" />
            <button type="button" className="btn-secondary" disabled={!text} onClick={() => downloadText(`${name}-${pod}.log`, text)}>
              Download
            </button>
          </div>

          {logsQ.isError ? (
            <ErrorState title="Could not load logs." error={logsQ.error} onRetry={() => logsQ.refetch()} retrying={logsQ.isFetching} />
          ) : logsQ.isLoading ? (
            <Skeleton rows={3} />
          ) : (
            <>
              {logs?.message && <p className="muted" role="status">{logs.message}</p>}
              {!logs?.message && notStarted && <p className="muted" role="status">The container has not started yet, so there is no output.</p>}
              {logs?.truncated && <p className="faint">Output was truncated; showing the last {tail} lines or fewer. Download for what is loaded.</p>}
              <pre
                ref={areaRef}
                className="mono log-view"
                role="log"
                aria-live="off"
                aria-label={`Logs of pod ${pod}`}
                tabIndex={0}
              >
                {lines && lines.length > 0 ? text : logs?.message || notStarted ? '' : '(no output)'}
              </pre>
            </>
          )}
        </div>
      )}
    </section>
  )
}

function EventsCard({ name }: { name: string }) {
  const now = useNow()
  const eventsQ = useQuery({ queryKey: ['job-events', name], queryFn: () => api.getJobEvents(name), refetchInterval: 15000, retry: 1 })
  const events: JobEvent[] = [...(eventsQ.data ?? [])].sort((a, b) => (Date.parse(b.lastSeen ?? '') || 0) - (Date.parse(a.lastSeen ?? '') || 0))
  return (
    <section className="card span3">
      <p className="eyebrow">RUNTIME</p>
      <h2 className="card-title">Events</h2>
      {eventsQ.isError && !eventsQ.data ? (
        <ErrorState title="Could not load events." error={eventsQ.error} onRetry={() => eventsQ.refetch()} retrying={eventsQ.isRefetching} />
      ) : eventsQ.isLoading ? (
        <Skeleton rows={2} />
      ) : events.length === 0 ? (
        <p className="faint">No events recorded for this job. Events are emitted by the Gryvia operator and the Kubernetes scheduler as it runs.</p>
      ) : (
        <div className="table-wrap">
          <table>
            <TableCaption>Kubernetes events for this job</TableCaption>
            <thead>
              <tr>
                <th scope="col">Type</th>
                <th scope="col">Reason</th>
                <th scope="col">Message</th>
                <th scope="col" className="num">
                  Count
                </th>
                <th scope="col">Last seen</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e, i) => (
                <tr key={`${e.object}-${e.reason}-${e.lastSeen}-${i}`}>
                  <td>
                    <span className={`pill ${e.type === 'Warning' ? 'warn' : ''}`}>{e.type || 'Normal'}</span>
                  </td>
                  <td>{e.reason}</td>
                  <td>{e.message}</td>
                  <td className="num">{e.count}</td>
                  <td className="faint" title={formatDate(e.lastSeen)}>
                    {formatRelative(e.lastSeen, now)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}

function ConditionsCard({ job }: { job: Parameters<typeof jobConditions>[0] }) {
  const conditions = jobConditions(job)
  if (conditions.length === 0) return null
  return (
    <section className="card span3">
      <p className="eyebrow">STATUS</p>
      <h2 className="card-title">Conditions</h2>
      <ol className="stack list-plain">
        {conditions.map((c, i) => (
          <li key={`${c.type}-${i}`} className="list-row">
            <span className={`dot ${c.status === 'True' ? 'ok' : c.status === 'False' ? 'warn' : ''}`} aria-hidden="true" />
            <div className="grow">
              <b>
                {c.type}: {c.status}
                {c.reason ? ` · ${c.reason}` : ''}
              </b>
              {c.message && <small>{c.message}</small>}
            </div>
            <span className="faint" title={formatDate(c.lastTransitionTime)}>
              {formatRelative(c.lastTransitionTime)}
            </span>
          </li>
        ))}
      </ol>
    </section>
  )
}
