// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import Layout from './Layout'
import { AuthContext, type AuthContextType, type UserInfo } from '@/lib/auth'

const user = (usingDefaultKey: boolean): UserInfo => ({
  authenticated: true, method: 'api_key', sub: 'api-key-user', email: '', name: 'admin',
  groups: [], org: '', tenantNamespaces: null, usingDefaultKey,
})

function renderLayout(u: UserInfo) {
  const value: AuthContextType = {
    isAuthenticated: true, isLoading: false, user: u, authConfig: null,
    loginWithApiKey: vi.fn(), loginWithPassword: vi.fn(), loginWithSSO: vi.fn(), handleOIDCCallback: vi.fn(),
    logout: vi.fn(), error: null,
  }
  return render(
    <MemoryRouter>
      <AuthContext.Provider value={value}>
        <Layout><p>page</p></Layout>
      </AuthContext.Provider>
    </MemoryRouter>,
  )
}

describe('Layout default-key notice', () => {
  beforeEach(() => {
    sessionStorage.clear()
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} }))
  })

  it('warns while the default lab key is in use and can be dismissed', async () => {
    renderLayout(user(true))
    expect(screen.getByText(/default lab key/i)).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }))
    expect(screen.queryByText(/default lab key/i)).toBeNull()
    expect(sessionStorage.getItem('gryvia_default_key_notice')).toBe('1')
  })

  it('stays quiet with a custom key', () => {
    renderLayout(user(false))
    expect(screen.queryByText(/default lab key/i)).toBeNull()
  })
})

describe('Layout role gating', () => {
  beforeEach(() => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} }))
  })

  it('hides admin groups from tenant users', () => {
    renderLayout({ ...user(false), role: 'tenant', tenant: 'acme' })
    expect(screen.getByRole('button', { name: 'Cloud' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Platform' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Observe' })).toBeNull()
  })

  it('shows them to admins and to old gateways without a role', () => {
    renderLayout({ ...user(false), role: 'admin' })
    expect(screen.getByRole('button', { name: 'Platform' })).toBeTruthy()
    renderLayout(user(false))
    expect(screen.getAllByRole('button', { name: 'Observe' }).length).toBe(2)
  })
})
