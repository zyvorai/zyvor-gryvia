// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Color is deviation (docs/design/APPLE-UX-CONTRACT.md): 'ok' is confirmed good,
// 'warn'/'bad' only when a value really deviates, 'idle' when there is no data yet.
export type Tone = 'ok' | 'warn' | 'bad' | 'idle'

/** Tone for a count where any nonzero value is a deviation. */
export function countTone(n: number | undefined, bad = Infinity): Tone {
  if (n === undefined || !Number.isFinite(n)) return 'idle'
  if (n >= bad) return 'bad'
  return n > 0 ? 'warn' : 'ok'
}
