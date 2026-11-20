// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Lets the axios interceptor (outside React) route the user through the router instead of a hard reload.
type Handler = (info: { reason: 'expired' }) => void

let onUnauthorized: Handler | undefined

export function setUnauthorizedHandler(handler: Handler | undefined): void {
  onUnauthorized = handler
}

export function notifyUnauthorized(): boolean {
  if (!onUnauthorized) return false
  onUnauthorized({ reason: 'expired' })
  return true
}
