// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import type { ReactNode } from 'react'

/** Text for screen readers only. */
export function SrOnly({ children }: { children: ReactNode }) {
  return <span className="sr-only">{children}</span>
}

/** A table caption that is announced but not shown. */
export function TableCaption({ children }: { children: ReactNode }) {
  return <caption className="sr-only">{children}</caption>
}
