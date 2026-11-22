// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import type { ReactNode } from 'react'

/** Content available to screen readers only (e.g. a table alternative to a chart). */
export default function VisuallyHidden({ children }: { children: ReactNode }) {
  return <div className="sr-only">{children}</div>
}
