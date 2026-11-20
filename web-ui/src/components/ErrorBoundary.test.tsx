// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ErrorBoundary from './ErrorBoundary'

function Boom({ message }: { message: string }): never {
  throw new Error(message)
}

describe('ErrorBoundary', () => {
  beforeEach(() => {
    sessionStorage.clear()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    vi.stubGlobal('location', { ...window.location, reload: vi.fn() })
  })

  it('offers Try again for ordinary errors', () => {
    render(<ErrorBoundary><Boom message="boom" /></ErrorBoundary>)
    expect(screen.getByText('This page hit a problem.')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy()
    expect(window.location.reload).not.toHaveBeenCalled()
  })

  it('reloads once and shows an update notice for a stale chunk', () => {
    render(<ErrorBoundary><Boom message="Failed to fetch dynamically imported module: /assets/x.js" /></ErrorBoundary>)
    expect(window.location.reload).toHaveBeenCalledTimes(1)
    expect(screen.getByText('Gryvia was updated.')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Reload' })).toBeTruthy()
  })
})
