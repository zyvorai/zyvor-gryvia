// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Port of netra's components/kit/PagePulse.tsx (same markup and classes, styled by
// netra-story.css). Differences: no sparkline series, no count-up animation, and the live
// pill takes a timestamp (react-query's dataUpdatedAt) instead of tracking a tick in an effect.
import { useNow } from '@/lib/useNow'
import { compact } from './format'
import type { Tone } from './tone'

export type PulseFigureSpec = {
  label: string
  value?: number | string
  format?: (n: number) => string
  tone?: Tone
}

export function PulseFigure({ label, value, format = compact, tone }: PulseFigureSpec) {
  const numeric = typeof value === 'number' && Number.isFinite(value)
  return (
    <div className={`kit-pulse__cell${tone ? ' tone-' + tone : ''}`}>
      <span>{label}</span>
      <b>{numeric ? format(value as number) : value === undefined || value === '' ? '—' : value}</b>
    </div>
  )
}

function ago(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  return `${Math.round(s / 3600)}h ago`
}

/** Live pill. `updatedAt` is when the data last refreshed (ms since epoch); 0/undefined = still loading. */
export function LivePill({ updatedAt, error }: { updatedAt?: number; error?: string }) {
  const now = useNow(1000)
  const at = updatedAt || undefined
  const state = error ? 'stale' : at ? 'live' : 'idle'
  const text =
    state === 'stale'
      ? `Stale${at ? ' · last ' + ago(now - at) : ''}`
      : state === 'live'
        ? `Live · updated ${ago(now - at!)}`
        : 'Loading…'
  return (
    <span className={`kit-live kit-live--${state}`} title={error || undefined}>
      <i aria-hidden="true" /> {text}
    </span>
  )
}

type PagePulseProps = {
  figures: PulseFigureSpec[]
  updatedAt?: number
  error?: string
  /** One plain-language sentence summarizing the page state. */
  headline?: string
  tone?: Tone
  /** Hide the live pill on on-demand (non-polling) results. */
  live?: boolean
}

export default function PagePulse({ figures, updatedAt, error, headline, tone, live = true }: PagePulseProps) {
  return (
    <section className="kit-pulse span3" aria-label="Live summary">
      <div className="kit-pulse__head">
        {headline && <h2 className={`kit-pulse__headline${tone ? ' tone-' + tone : ''}`}>{headline}</h2>}
        {live && <LivePill updatedAt={updatedAt} error={error} />}
      </div>
      <div className="kit-pulse__grid">
        {figures.map((f) => (
          <PulseFigure key={f.label} {...f} />
        ))}
      </div>
    </section>
  )
}
