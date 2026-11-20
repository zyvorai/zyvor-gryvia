// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { notify } from '@/lib/notify'

/** Small button that copies a value to the clipboard and says so. */
export default function CopyButton({ value, label }: { value: string; label: string }) {
  const copy = async (e: React.MouseEvent) => {
    e.stopPropagation()
    try {
      await navigator.clipboard.writeText(value)
      notify.success(`Copied ${label}`)
    } catch (err) {
      notify.error(`Could not copy ${label}`, err)
    }
  }
  return (
    <button type="button" className="btn-secondary" onClick={copy} aria-label={`Copy ${label}`}>
      Copy
    </button>
  )
}
