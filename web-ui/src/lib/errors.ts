// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import axios from 'axios'

/**
 * Human-readable message for a failed API call: the gateway's `detail` (a string, or a list of
 * validation errors) when there is one, otherwise the error's own message.
 */
export function errorMessage(err: unknown): string {
  if (axios.isAxiosError(err)) {
    const detail = (err.response?.data as { detail?: unknown } | undefined)?.detail
    if (typeof detail === 'string' && detail) return detail
    if (Array.isArray(detail) && detail.length > 0) {
      const first = detail[0] as { msg?: unknown; loc?: unknown[] }
      const where = Array.isArray(first.loc) ? first.loc.filter((p) => p !== 'body').join('.') : ''
      const msg = typeof first.msg === 'string' ? first.msg : 'invalid request'
      return where ? `${where}: ${msg}` : msg
    }
  }
  return err instanceof Error ? err.message : 'Unknown error'
}
