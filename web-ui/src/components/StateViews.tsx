// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import type { ReactNode } from 'react'
import { errorMessage } from '@/lib/errors'

/** A failed load: says what happened (with the gateway's reason) and lets the user retry. */
export function ErrorState({ title = 'Could not load this data.', error, onRetry, retrying }: { title?: string; error?: unknown; onRetry?: () => void; retrying?: boolean }) {
  return (
    <div className="warning" role="alert">
      <strong>{title}</strong>
      {error !== undefined && <div>{errorMessage(error)}</div>}
      {onRetry && (
        <div className="toolbar mt-10">
          <button type="button" className="btn-secondary" onClick={onRetry} disabled={retrying}>
            {retrying ? 'Retrying…' : 'Retry'}
          </button>
        </div>
      )}
    </div>
  )
}

/** Nothing here yet: says why, names what produces the data, and offers a next step. */
export function EmptyState({ title, children, action }: { title: string; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="empty-state">
      <strong>{title}</strong>
      {children && <div>{children}</div>}
      {action && <div className="toolbar toolbar-center mt-10">{action}</div>}
    </div>
  )
}

/** Placeholder rows while data loads (avoids layout jumps and false zeros). */
export function Skeleton({ rows = 3 }: { rows?: number }) {
  return (
    <div className="stack" role="status" aria-label="Loading" aria-busy="true">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton" style={{ height: 20, width: `${90 - i * 12}%` }} />
      ))}
    </div>
  )
}
