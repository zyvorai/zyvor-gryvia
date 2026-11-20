// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

/** Progress bar with the semantics a screen reader needs (value + label), colour is a hint, not the only signal. */
export default function Progress({ value, max = 100, label, tone }: { value: number; max?: number; label: string; tone?: 'ok' | 'warn' | 'bad' }) {
  const pct = max > 0 ? Math.max(0, Math.min(100, (value / max) * 100)) : 0
  return (
    <div className={`progress${tone ? ' ' + tone : ''}`} role="progressbar" aria-label={label} aria-valuemin={0} aria-valuemax={max} aria-valuenow={Math.round(value)}>
      <span style={{ width: `${pct}%` }} />
    </div>
  )
}
