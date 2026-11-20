// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState } from 'react'
import axios from 'axios'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { saveBlob } from '@/lib/download'
import { errorMessage } from '@/lib/errors'
import { ErrorState, Skeleton } from '@/components/StateViews'

function failure(error: unknown, namespace: string) {
  const status = axios.isAxiosError(error) ? error.response?.status : undefined
  if (status === 403) return `Your account cannot read diagnosis data for namespace ${namespace}.`
  if (status === 503) return `${errorMessage(error)}. Nothing could be measured; this does not mean the job is healthy.`
  return errorMessage(error)
}

const severityClass = { critical: 'err', warning: 'warn', info: 'ok' } as const

/**
 * Evidence-backed bottleneck findings for the job, merged by the gateway from every reachable node.
 * Loaded on request only (each load fans out to every collector). The card never presents a result
 * as complete unless the gateway says every expected node reported and nothing was unavailable; the
 * completeness block is always shown. All text comes from the cluster and is rendered as plain text.
 */
export default function FlightDiagnosis({ namespace, job }: { namespace: string; job: string }) {
  const [enabled, setEnabled] = useState(false)
  const query = useQuery({
    queryKey: ['flight-diagnosis', namespace, job],
    queryFn: () => api.getFlightDiagnosis(job, namespace),
    enabled,
    retry: false,
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    refetchInterval: false,
  })
  const data = query.data
  const mc = data?.measurementCompleteness
  const incomplete = data ? data.partial || !mc?.complete : false

  return (
    <section className="card span3" aria-label="Bottleneck diagnosis">
      <p className="eyebrow">DIAGNOSIS</p>
      <h2 className="card-title">Bottleneck diagnosis</h2>
      <p className="faint">
        Rule-based findings from Flight Recorder events, fabric signals and node cgroup counters. Telemetry that could not be collected is listed as unavailable and is never counted as healthy.
      </p>
      <div className="toolbar">
        <button type="button" className="btn-secondary" disabled={query.isFetching} onClick={() => (enabled ? void query.refetch() : setEnabled(true))}>
          {query.isFetching ? 'Diagnosing…' : data ? 'Refresh diagnosis' : 'Load diagnosis'}
        </button>
        {data && (
          <button
            type="button"
            className="btn-secondary"
            onClick={() => saveBlob(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }), `diagnosis-${namespace}-${job}.json`)}
          >
            Download diagnosis
          </button>
        )}
      </div>
      {query.isFetching && !data && <Skeleton rows={2} />}
      {query.isError && (
        <ErrorState title="Could not load the diagnosis." error={new Error(failure(query.error, namespace))} onRetry={() => void query.refetch()} retrying={query.isFetching} />
      )}
      {data && mc && (
        <>
          <p role="status">
            <span className={`pill ${incomplete ? 'warn' : 'ok'}`}>{incomplete ? 'Incomplete measurement' : 'Every expected node reported, nothing unavailable'}</span> {data.summary}
          </p>
          {data.findings.length > 0 && (
            <ol aria-label="Findings, most significant first">
              {data.findings.map((f) => (
                <li key={f.kind}>
                  <strong>{f.kind}</strong> <span className={`pill ${severityClass[f.severity] ?? 'warn'}`}>{f.severity}</span> <span className="faint">{f.confidence} confidence, nodes: {f.nodes.join(', ')}</span>
                  <p>{f.summary}</p>
                  <details>
                    <summary>Evidence ({f.evidence.length}) and what was not measured ({f.whatWasNotMeasured.length})</summary>
                    <ul>
                      {f.evidence.map((e, i) => (
                        <li key={`${e.node}-${e.metric}-${i}`}>
                          {e.node}: {e.source} {e.metric} = {e.value} (window {e.window})
                        </li>
                      ))}
                    </ul>
                    {f.whatWasNotMeasured.length > 0 && (
                      <ul aria-label={`Not measured for ${f.kind}`}>
                        {f.whatWasNotMeasured.map((m, i) => (
                          <li key={i} className="faint">
                            {m}
                          </li>
                        ))}
                      </ul>
                    )}
                  </details>
                </li>
              ))}
            </ol>
          )}
          <details open={incomplete}>
            <summary>
              Measurement completeness: {mc.nodesReporting} of {String(mc.nodesExpected)} expected node(s) reported
            </summary>
            <ul>
              <li>Probes attached: {mc.probesAttached}; skipped: {mc.probesSkipped.length}</li>
              <li>Dropped events before analysis: {mc.droppedEvents.total}</li>
              <li>Sampling ratio: {mc.sampling.ratio}</li>
              {mc.missingNodes.length > 0 && <li>Missing nodes: {mc.missingNodes.join(', ')}</li>}
            </ul>
            {mc.reasons.length > 0 && (
              <ul aria-label="Why the measurement is incomplete">
                {mc.reasons.map((r, i) => (
                  <li key={i}>{r}</li>
                ))}
              </ul>
            )}
          </details>
          <details open={data.unavailable.length > 0}>
            <summary>Unavailable telemetry ({data.unavailable.length})</summary>
            {data.unavailable.length === 0 ? (
              <p className="faint">None reported.</p>
            ) : (
              <ul>
                {data.unavailable.map((u, i) => (
                  <li key={`${u.node}-${u.signal}-${i}`}>
                    {u.node}: {u.signal}: {u.reason}
                  </li>
                ))}
              </ul>
            )}
          </details>
          <details>
            <summary>Measured signals</summary>
            <ul>
              {Object.entries(data.measured).map(([node, signals]) => (
                <li key={node}>
                  {node}: {signals.length ? signals.join(', ') : 'none'}
                </li>
              ))}
            </ul>
          </details>
        </>
      )}
    </section>
  )
}
