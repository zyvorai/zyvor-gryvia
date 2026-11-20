// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { engineLabel, formatCount, formatMs, formatRatio, inferenceRows, NOT_MEASURED } from '@/lib/inference'
import { TableCaption } from '@/components/TableCaption'

/**
 * The serving engine's own latency for a job: time to first token, inter-token latency and engine
 * queue time, read from the engine's metrics by the node collectors (opt-in) and published on the
 * job's GryviaFabricSignal. It renders nothing unless data exists: no scraper, no signal object or
 * an unreachable gateway all leave the page as it was. Kernel probes cannot see tokens, so this is
 * the engine's report, not a network measurement; the network accept wait is shown separately.
 */
export default function InferencePanel({ namespace, job }: { namespace: string; job: string }) {
  const q = useQuery({
    queryKey: ['inference-latency', namespace, job],
    queryFn: () => api.getInferenceLatency(job, namespace),
    retry: false,
    staleTime: 30_000,
    refetchInterval: 30_000,
    refetchOnWindowFocus: false,
  })
  const d = q.data
  if (!d || !d.available) return null
  const rows = inferenceRows(d)

  return (
    <section className="card span3" aria-label="Inference latency">
      <p className="eyebrow">INFERENCE</p>
      <h2 className="card-title">Inference latency</h2>
      <p className="faint">
        Reported by <strong>{engineLabel(d.engine)}</strong> itself over roughly the last five minutes (p99 of per-interval histogram deltas), scraped by the node collector. It is
        not measured by the network probes. {d.updatedAt ? `Updated ${d.updatedAt}.` : ''}
      </p>
      <div className="table-wrap">
        <table>
          <TableCaption>Serving engine latency for this job</TableCaption>
          <thead>
            <tr>
              <th scope="col">Metric</th>
              <th scope="col">Value</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.key}>
                <th scope="row">{r.label}</th>
                <td className={r.measured ? undefined : 'faint'}>
                  {r.value}
                  {r.measured && r.hint ? <span className="faint"> ({r.hint})</span> : null}
                </td>
              </tr>
            ))}
            <tr>
              <th scope="row">Requests waiting</th>
              <td className={d.requestsWaiting === undefined ? 'faint' : undefined}>{formatCount(d.requestsWaiting)}</td>
            </tr>
            <tr>
              <th scope="row">KV cache usage</th>
              <td className={d.kvCacheUsage === undefined ? 'faint' : undefined}>{formatRatio(d.kvCacheUsage)}</td>
            </tr>
            <tr>
              <th scope="row">Network accept wait, p99</th>
              <td className={d.inferWaitP99ms === undefined ? 'faint' : undefined}>
                {d.inferWaitP99ms === undefined ? NOT_MEASURED : formatMs(d.inferWaitP99ms)}
                <span className="faint"> (accept to first read on the socket; not queue time)</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <p className="faint">
        “{NOT_MEASURED}” means the engine does not export the metric, or no request finished in the window. It does not mean zero.
      </p>
    </section>
  )
}
