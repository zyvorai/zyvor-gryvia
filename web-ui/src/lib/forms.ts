// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Pure form helpers shared by the create dialogs; the patterns mirror the gateway's validation.

export const K8S_NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
export const STORAGE_PATTERN = /^[1-9][0-9]{0,5}(Mi|Gi|Ti)$/
export const IDLE_TIMEOUT_PATTERN = /^[1-9]\d{0,4}[mh]$/

/** Inline validation message for a Kubernetes-style resource name (null when valid). */
export function nameError(name: string): string | null {
  if (name === '') return 'Enter a name.'
  if (name.length > 63) return 'Use 63 characters or fewer.'
  if (!K8S_NAME.test(name)) return 'Use lowercase letters, digits and hyphens; start and end with a letter or digit.'
  return null
}

/** Strict integer parse of a text input; null for blank, decimals, exponents or junk. */
export function parseIntStrict(raw: string): number | null {
  const t = raw.trim()
  return /^-?\d+$/.test(t) ? Number(t) : null
}

/** Validation message for an integer field within [min, max] (null when valid). */
export function intError(raw: string, min: number, max: number): string | null {
  const n = parseIntStrict(raw)
  if (n === null) return 'Enter a whole number.'
  if (n < min || n > max) return `Enter a number from ${min} to ${max}.`
  return null
}

/** "50" + "Gi" -> "50Gi"; null when the result is not a valid Kubernetes quantity for the gateway. */
export function buildStorage(amount: string, unit: 'Mi' | 'Gi' | 'Ti' | string): string | null {
  const v = `${amount.trim()}${unit}`
  return STORAGE_PATTERN.test(v) ? v : null
}

/** "30" + "m" -> "30m"; null when invalid. */
export function buildIdleTimeout(amount: string, unit: 'm' | 'h' | string): string | null {
  const v = `${amount.trim()}${unit}`
  return IDLE_TIMEOUT_PATTERN.test(v) ? v : null
}

/**
 * aria wiring for a field: invalid when it has an error, described by `${id}-msg` when there is
 * an error or a hint to read. Render the message element with that same id.
 */
export function fieldAria(id: string, error?: string | null, hasHint = false): { 'aria-invalid': boolean; 'aria-describedby'?: string } {
  return { 'aria-invalid': !!error, 'aria-describedby': error || hasHint ? `${id}-msg` : undefined }
}
