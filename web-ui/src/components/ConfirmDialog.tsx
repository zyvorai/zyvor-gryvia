// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import type { ReactNode } from 'react'
import Modal from './Modal'

type ConfirmDialogProps = {
  title: string
  /** What will happen, naming the resource: "Delete workspace ui-audit-ws? Its 50Gi volume is deleted too." */
  children: ReactNode
  confirmLabel: string
  onConfirm: () => void
  onCancel: () => void
  /** danger = destructive (red, focus starts on Cancel); primary = a consequential but safe action. */
  tone?: 'danger' | 'primary'
  busy?: boolean
}

/** In-app replacement for window.confirm(): accessible, names the consequence, shows a busy state. */
export default function ConfirmDialog({ title, children, confirmLabel, onConfirm, onCancel, tone = 'danger', busy = false }: ConfirmDialogProps) {
  return (
    <Modal title={title} onClose={onCancel}>
      <div className="stack">
        <div className="muted">{children}</div>
        <div className="toolbar">
          <button type="button" className="btn-secondary" data-autofocus={tone === 'danger' ? '' : undefined} onClick={onCancel} disabled={busy}>
            Cancel
          </button>
          <button type="button" className={tone === 'danger' ? 'danger' : 'primary'} onClick={onConfirm} disabled={busy}>
            {busy ? 'Working…' : confirmLabel}
          </button>
        </div>
      </div>
    </Modal>
  )
}
