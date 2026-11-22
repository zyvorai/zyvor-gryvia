// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import Login from './Login'
import { AuthContext, kubectlCommand, type AuthConfig, type AuthContextType } from '@/lib/auth'

function renderLogin(authConfig: AuthConfig | null, overrides: Partial<AuthContextType> = {}) {
  const value: AuthContextType = {
    isAuthenticated: false, isLoading: false, user: null, authConfig,
    loginWithApiKey: vi.fn(), loginWithPassword: vi.fn().mockResolvedValue(undefined), loginWithSSO: vi.fn(),
    handleOIDCCallback: vi.fn(), logout: vi.fn(), error: null, ...overrides,
  }
  render(
    <MemoryRouter>
      <AuthContext.Provider value={value}>
        <Login />
      </AuthContext.Provider>
    </MemoryRouter>,
  )
  return value
}

const config: AuthConfig = {
  oidcEnabled: false,
  apiKeyEnabled: true,
  credentials: { username: 'admin', secret: 'my-key', key: 'GRYVIA_API_KEY', namespace: 'ai-system' },
  instance: { product: 'Gryvia', version: '1.0.0', namespace: 'ai-system' },
}

describe('Login', () => {
  beforeEach(() => localStorage.clear())

  it('says where the admin password lives, with the Secret from the gateway', () => {
    renderLogin(config)
    const cmd = "kubectl -n ai-system get secret my-key -o jsonpath='{.data.GRYVIA_API_KEY}' | base64 -d"
    expect(kubectlCommand(config.credentials!)).toBe(cmd)
    expect(screen.getByLabelText('Command that prints the admin password').textContent).toBe(cmd)
    expect(screen.getByText('v1.0.0')).toBeTruthy()
  })

  it('says what Gryvia does and the project goals', () => {
    renderLogin(config)
    expect(screen.getByRole('heading', { level: 1 }).textContent).toContain('Gryvia is its scheduler.')
    const about = screen.getByRole('region', { name: 'What Gryvia does' })
    for (const t of ['GPU-aware scheduling', 'GPU as a Service', 'Models and inference', 'Network intelligence',
      'Sovereign AI OS', 'Workflows and experiments']) {
      expect(about.textContent).toContain(t)
    }
    expect(screen.getByText('Source-available under the Zyvor Production Licence v2.0')).toBeTruthy()
  })

  it('focuses the username without leaving the hero', () => {
    const focus = vi.spyOn(HTMLElement.prototype, 'focus')
    renderLogin(config)
    expect(document.activeElement?.id).toBe('username')
    expect(focus).toHaveBeenCalledWith({ preventScroll: true })
    focus.mockRestore()
  })

  it('falls back to the chart defaults on an older gateway', () => {
    renderLogin({ oidcEnabled: false, apiKeyEnabled: true })
    expect(screen.getByLabelText('Command that prints the admin password').textContent).toContain(
      'kubectl -n gryvia-system get secret gryvia-api-key',
    )
  })

  it('asks for the username, then the password, and signs in', async () => {
    const auth = renderLogin(config)
    expect(screen.queryByLabelText('Password')).toBeNull()
    await userEvent.type(screen.getByLabelText('Username'), 'admin')
    await userEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(screen.getByText('Enter the password for')).toBeTruthy()
    expect(screen.queryByLabelText('Command that prints the admin password')).toBeNull()

    const pw = screen.getByLabelText('Password') as HTMLInputElement
    await userEvent.type(pw, 's3cret')
    expect(pw.type).toBe('password')
    await userEvent.click(screen.getByRole('button', { name: 'Show password' }))
    expect(pw.type).toBe('text')

    await userEvent.click(screen.getByRole('button', { name: /^sign in/i }))
    expect(auth.loginWithPassword).toHaveBeenCalledWith('admin', 's3cret')
    expect(localStorage.getItem('gryvia-saved-login')).toBeNull()
  })

  it('remembers only the username', async () => {
    renderLogin(config)
    await userEvent.type(screen.getByLabelText('Username'), 'admin')
    await userEvent.click(screen.getByRole('button', { name: /continue/i }))
    await userEvent.type(screen.getByLabelText('Password'), 's3cret')
    await userEvent.click(screen.getByLabelText('Remember me on this device'))
    await userEvent.click(screen.getByRole('button', { name: /^sign in/i }))
    expect(JSON.parse(localStorage.getItem('gryvia-saved-login')!)).toEqual({ username: 'admin' })
  })

  it('can go back to change the account', async () => {
    renderLogin(config)
    await userEvent.type(screen.getByLabelText('Username'), 'ops')
    await userEvent.click(screen.getByRole('button', { name: /continue/i }))
    await userEvent.click(screen.getByRole('button', { name: 'Change account' }))
    expect(screen.getByLabelText('Username')).toBeTruthy()
  })

  it('shows SSO and hides the password hint when only SSO is on', async () => {
    const auth = renderLogin({ oidcEnabled: true, apiKeyEnabled: false })
    expect(screen.queryByLabelText('Username')).toBeNull()
    expect(screen.queryByLabelText('Command that prints the admin password')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Sign in with SSO' }))
    expect(auth.loginWithSSO).toHaveBeenCalled()
  })

  it('reports a failed sign-in', async () => {
    renderLogin(config, { loginWithPassword: vi.fn().mockRejectedValue(new Error('Invalid credentials')) })
    await userEvent.type(screen.getByLabelText('Username'), 'admin')
    await userEvent.click(screen.getByRole('button', { name: /continue/i }))
    await userEvent.type(screen.getByLabelText('Password'), 'nope')
    await userEvent.click(screen.getByRole('button', { name: /^sign in/i }))
    expect((await screen.findByRole('alert')).textContent).toContain('Invalid credentials')
  })
})
