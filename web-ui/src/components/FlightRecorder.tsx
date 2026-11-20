// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState } from 'react'
import axios from 'axios'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { saveBlob } from '@/lib/download'
import { errorMessage } from '@/lib/errors'
import { ErrorState, Skeleton } from '@/components/StateViews'
import { TableCaption } from '@/components/TableCaption'

const MAX_ROWS = 100

function failure(error: unknown, namespace: string) {
  const status = axios.isAxiosError(error) ? error.response?.status : undefined
  if (status === 403) return `Your account cannot read Flight Recorder data for namespace ${namespace}.`
  if (status === 503) return `${errorMessage(error)}. Recorder data cannot be shown until collectors are reachable; this does not mean the job is healthy.`
  return errorMessage(error)
}

/**
 * The job's recent TCP, CUDA and NCCL observations, merged by the gateway from every reachable
 * node collector. Loaded on request only (each load fans out to every collector), never polled.
 * All text comes from the cluster and is rendered as plain text.
 */
export default function FlightRecorder({ namespace, job }: { namespace: string; job: string }) {
  const [enabled, setEnabled] = useState(false)
  const report = useQuery({
    queryKey: ['flight', namespace, job],
    queryFn: () => api.getFlightReport(job, namespace),
    enabled,
    retry: false,
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    refetchInterval: false,
  })
  const data = report.data
  const partial = data ? !data.coverage.complete || data.truncated : false
  const events = data?.events ?? []
  const shown = events.slice(-MAX_ROWS).reverse()

  return (
    <section className="card span3" aria-label="Flight Recorder">
      <p className="eyebrow">OBSERVATIONS</p>
      <h2 className="card-title">Flight Recorder</h2>
      <p className="faint">
        Recent TCP, CUDA and NCCL events attributed to this job by the node collectors. Missing events do not establish healthy GPU or network behavior.
      </p>
      <div className="toolbar">
        <button type="button" className="btn-secondary" disabled={report.isFetching} onClick={() => (enabled ? void report.refetch() : setEnabled(true))}>
          {report.isFetching ? 'Collecting observations…' : data ? 'Refresh observations' : 'Load observations'}
        </button>
        {data && (
          <button
            type="button"
            className="btn-secondary"
            onClick={() => saveBlob(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }), `flight-${namespace}-${job}.json`)}
          >
            Download report
          </button>
        )}
      </div>
      {report.isFetching && !data && <Skeleton rows={2} />}
      {report.isError && (
        <ErrorState title="Could not load Flight Recorder data." error={new Error(failure(report.error, namespace))} onRetry={() => void report.refetch()} retrying={report.isFetching} />
      )}
      {data && (
        <>
          <p role="status">
            <span className={`pill ${partial ? 'warn' : 'ok'}`}>{partial ? 'Partial coverage' : 'All discovered collectors answered'}</span>{' '}
            {data.coverage.reachable} of {data.coverage.total} discovered collectors reachable; {data.coverage.reporting} returned events for this job.
            {data.truncated && ' Events were dropped by a limit, so older events may be missing.'}
            {!partial && ' Nodes without a running collector are not counted.'}
          </p>
          {data.findings.length === 0 ? (
            <p>No findings in the retained sample.</p>
          ) : (
            <ul>
              {data.findings.map((f, i) => (
                <li key={`${f.node}-${f.code}-${i}`}>
                  <strong>{f.node}: </strong>
                  {f.evidence}
                </li>
              ))}
            </ul>
          )}
          <details>
            <summary>Observed event counts</summary>
            {Object.keys(data.counts).length === 0 ? (
              <p className="faint">None.</p>
            ) : (
              <dl>
                {Object.entries(data.counts).map(([kind, count]) => (
                  <div key={kind}>
                    <dt>{kind}</dt>
                    <dd>{count}</dd>
                  </div>
                ))}
              </dl>
            )}
          </details>
          <details>
            <summary>
              Event timeline ({events.length}
              {events.length > shown.length ? `, newest ${shown.length} shown; download for all` : ''})
            </summary>
            {events.length === 0 ? (
              <p className="faint">No events.</p>
            ) : (
              <div className="table-wrap">
                <table>
                  <TableCaption>Flight Recorder events, newest first</TableCaption>
                  <thead>
                    <tr>
                      <th scope="col">Time</th>
                      <th scope="col">Node</th>
                      <th scope="col">Pod</th>
                      <th scope="col">Rank</th>
                      <th scope="col">Event</th>
                    </tr>
                  </thead>
                  <tbody>
                    {shown.map((e, i) => (
                      <tr key={`${e.time}-${e.node}-${i}`}>
                        <td>{e.time}</td>
                        <td>{e.node}</td>
                        <td>{e.identity?.pod ?? 'Unknown'}</td>
                        <td>{e.identity?.rank ?? 'Unknown'}</td>
                        <td>{e.operation ? `${e.kind} (${e.operation})` : e.kind}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </details>
        </>
      )}
    </section>
  )
}
