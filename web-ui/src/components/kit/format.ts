// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

/** Compact number: 1.2K, 3.4M, integers under 100 as-is. */
export function compact(n: number): string {
  if (!Number.isFinite(n)) return '—'
  const abs = Math.abs(n)
  if (abs >= 1e9) return (n / 1e9).toFixed(1) + 'B'
  if (abs >= 1e6) return (n / 1e6).toFixed(1) + 'M'
  if (abs >= 1e4) return (n / 1e3).toFixed(1) + 'K'
  if (abs >= 100) return Math.round(n).toLocaleString()
  return n.toFixed(abs < 10 && n !== 0 && !Number.isInteger(n) ? 1 : 0)
}
