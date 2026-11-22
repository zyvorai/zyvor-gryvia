// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useEffect } from 'react'

/** Sets the browser tab/history title to "<title> · Gryvia" while the page is mounted. */
export function useDocumentTitle(title: string): void {
  useEffect(() => {
    const previous = document.title
    document.title = title ? `${title} · Gryvia` : 'Gryvia'
    return () => {
      document.title = previous
    }
  }, [title])
}
