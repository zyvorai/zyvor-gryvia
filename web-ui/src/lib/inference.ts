// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import type { InferenceLatency } from '@/lib/api'

/** "not measured" is shown for a figure the engine does not export or that has no data in the window. */
export const NOT_MEASURED = 'not measured'

/** Milliseconds, human sized: 0.86 ms, 72 ms, 2.25 s. */
export function formatMs(ms: number | undefined): string {
  if (ms === undefined || !Number.isFinite(ms) || ms < 0) return NOT_MEASURED
  if (ms < 1) return `${ms.toFixed(2)} ms`
  if (ms < 100) return `${Number(ms.toFixed(1))} ms`
  if (ms < 1000) return `${Math.round(ms)} ms`
  return `${Number((ms / 1000).toFixed(2))} s`
}

export interface InferenceRow {
  key: string
  label: string
  value: string
  measured: boolean
  hint?: string
}

const row = (key: string, label: string, ms: number | undefined, hint?: string): InferenceRow => ({
  key,
  label,
  value: formatMs(ms),
  measured: ms !== undefined && Number.isFinite(ms) && ms >= 0,
  hint,
})

/**
 * The rows of the Inference panel. Time to first token, inter-token latency and queue time are
 * always listed so an engine that does not export one (Triton has no tokens) reads "not measured"
 * instead of the row silently missing. Counter-only engines (Triton without summary latencies) show
 * a mean, labelled as one; a mean is never presented as a p99.
 */
export function inferenceRows(d: InferenceLatency): InferenceRow[] {
  const rows: InferenceRow[] = [
    row('ttft', 'Time to first token, p99', d.ttftP99ms),
    row('itl', 'Inter-token latency, p99', d.itlP99ms, d.engine === 'tgi' ? 'p99 of the per-request mean time per token' : undefined),
    row('queue', 'Engine queue time, p99', d.queueTimeP99ms),
    row('e2e', 'End-to-end request latency, p99', d.e2eP99ms),
  ]
  if (d.queueTimeP99ms === undefined && d.queueTimeMeanMs !== undefined) {
    rows.push(row('queueMean', 'Engine queue time, mean', d.queueTimeMeanMs, 'this engine exports only cumulative durations'))
  }
  if (d.e2eP99ms === undefined && d.e2eMeanMs !== undefined) {
    rows.push(row('e2eMean', 'End-to-end request latency, mean', d.e2eMeanMs, 'this engine exports only cumulative durations'))
  }
  return rows
}

export function formatCount(v: number | undefined): string {
  return v === undefined || !Number.isFinite(v) ? NOT_MEASURED : String(Math.round(v))
}

export function formatRatio(v: number | undefined): string {
  return v === undefined || !Number.isFinite(v) ? NOT_MEASURED : `${Math.round(Math.max(0, Math.min(1, v)) * 100)}%`
}

/** Engine names arrive from the cluster: only known names are shown verbatim. */
export function engineLabel(engine: string | undefined): string {
  switch (engine) {
    case 'vllm':
      return 'vLLM'
    case 'triton':
      return 'NVIDIA Triton'
    case 'tgi':
      return 'HuggingFace TGI'
    case 'mixed':
      return 'Several engines (worst replica shown)'
    default:
      return 'Unknown engine'
  }
}
