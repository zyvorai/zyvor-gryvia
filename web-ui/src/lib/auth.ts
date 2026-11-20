// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { createContext, useContext } from 'react'
import axios from 'axios'

// ── Types ────────────────────────────────────────────────────────────

export interface AuthConfig {
  oidcEnabled: boolean
  apiKeyEnabled: boolean
  issuer?: string
  clientId?: string
  authorizationEndpoint?: string
  tokenEndpoint?: string
  scopes?: string
  error?: string
  /** Where an operator reads the admin key (never the key itself). Absent when API-key login is off. */
  credentials?: { username: string; secret: string; key: string; namespace: string }
  instance?: { product: string; version: string; namespace: string }
}

export interface UserInfo {
  authenticated: boolean
  method: 'oidc' | 'api_key'
  sub: string
  email: string
  name: string
  groups: string[]
  org: string
  tenantNamespaces: string[] | null
  /** 'admin' sees the whole platform; 'tenant' only its own tenant. Absent on older gateways (treated as admin). */
  role?: 'admin' | 'tenant'
  /** The tenant a tenant user belongs to. */
  tenant?: string | null
  tenants?: string[]
  /** True while the gateway still uses the well-known lab key. */
  usingDefaultKey?: boolean
}

export interface AuthContextType {
  isAuthenticated: boolean
  isLoading: boolean
  user: UserInfo | null
  authConfig: AuthConfig | null
  loginWithApiKey: (apiKey: string) => Promise<void>
  loginWithPassword: (username: string, password: string) => Promise<void>
  loginWithSSO: () => void
  handleOIDCCallback: (code: string) => Promise<void>
  logout: () => void
  error: string | null
}

// ── Constants ────────────────────────────────────────────────────────

const TOKEN_KEY = 'gryvia_token'
const AUTH_METHOD_KEY = 'gryvia_auth_method'
const OIDC_STATE_KEY = 'gryvia_oidc_state'
const OIDC_VERIFIER_KEY = 'gryvia_oidc_verifier'

// ── Helpers ──────────────────────────────────────────────────────────

function generateRandomString(length: number): string {
  const array = new Uint8Array(length)
  crypto.getRandomValues(array)
  return Array.from(array, (b) => b.toString(36).padStart(2, '0'))
    .join('')
    .slice(0, length)
}

async function sha256(plain: string): Promise<ArrayBuffer> {
  const encoder = new TextEncoder()
  return crypto.subtle.digest('SHA-256', encoder.encode(plain))
}

function base64URLEncode(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer)
  let binary = ''
  bytes.forEach((b) => (binary += String.fromCharCode(b)))
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

/** The command an operator runs to print the admin key from its Secret. */
export function kubectlCommand(c: { secret: string; key: string; namespace: string }): string {
  return `kubectl -n ${c.namespace} get secret ${c.secret} -o jsonpath='{.data.${c.key}}' | base64 -d`
}

export function getStoredToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function getStoredAuthMethod(): string | null {
  return localStorage.getItem(AUTH_METHOD_KEY)
}

// ── Context ──────────────────────────────────────────────────────────

export const AuthContext = createContext<AuthContextType>({
  isAuthenticated: false,
  isLoading: true,
  user: null,
  authConfig: null,
  loginWithApiKey: async () => {},
  loginWithPassword: async () => {},
  loginWithSSO: () => {},
  handleOIDCCallback: async () => {},
  logout: () => {},
  error: null,
})

export function useAuth(): AuthContextType {
  return useContext(AuthContext)
}

// ── Provider implementation (used by AuthProvider component) ─────────

export async function fetchAuthConfig(): Promise<AuthConfig> {
  const { data } = await axios.get<AuthConfig>('/api/auth/config')
  return data
}

/** Exchanges the dashboard credentials for the bearer token; the gateway validates them. */
export async function loginWithCredentials(username: string, password: string): Promise<string> {
  const { data } = await axios.post<{ token: string }>('/api/auth/login', { username, password })
  return data.token
}

/** Human message for a failed sign-in request. */
export function loginErrorMessage(err: unknown): string {
  const status = (err as { response?: { status?: number } })?.response?.status
  if (status === 401 || status === 403 || status === 422) return 'Wrong username or password.'
  if (status === 429) return 'Too many sign-in attempts. Wait a minute and try again.'
  return 'Could not reach the API gateway. Check the URL and try again.'
}

export async function fetchUserInfo(token: string): Promise<UserInfo> {
  const { data } = await axios.get<UserInfo>('/api/auth/me', {
    headers: { Authorization: `Bearer ${token}` },
  })
  return data
}

export async function exchangeCodeForToken(
  code: string,
  tokenEndpoint: string,
  clientId: string,
  codeVerifier: string,
): Promise<string> {
  const redirectUri = `${window.location.origin}/auth/callback`

  const params = new URLSearchParams()
  params.set('grant_type', 'authorization_code')
  params.set('code', code)
  params.set('redirect_uri', redirectUri)
  params.set('client_id', clientId)
  params.set('code_verifier', codeVerifier)

  const { data } = await axios.post(tokenEndpoint, params.toString(), {
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
  })

  // Prefer id_token for OIDC, fall back to access_token
  return data.id_token || data.access_token
}

export function buildAuthorizationURL(config: AuthConfig): {
  url: string
  state: string
  verifier: string
} {
  const state = generateRandomString(32)
  const verifier = generateRandomString(64)

  // We'll compute the code challenge synchronously-ish; store and redirect
  const redirectUri = `${window.location.origin}/auth/callback`

  const params = new URLSearchParams({
    response_type: 'code',
    client_id: config.clientId || '',
    redirect_uri: redirectUri,
    scope: config.scopes || 'openid profile email',
    state,
  })

  return {
    url: `${config.authorizationEndpoint}?${params.toString()}`,
    state,
    verifier,
  }
}

export async function buildAuthorizationURLWithPKCE(config: AuthConfig): Promise<{
  url: string
  state: string
  verifier: string
}> {
  const state = generateRandomString(32)
  const verifier = generateRandomString(64)

  const challengeBuffer = await sha256(verifier)
  const codeChallenge = base64URLEncode(challengeBuffer)

  const redirectUri = `${window.location.origin}/auth/callback`

  const params = new URLSearchParams({
    response_type: 'code',
    client_id: config.clientId || '',
    redirect_uri: redirectUri,
    scope: config.scopes || 'openid profile email',
    state,
    code_challenge: codeChallenge,
    code_challenge_method: 'S256',
  })

  return {
    url: `${config.authorizationEndpoint}?${params.toString()}`,
    state,
    verifier,
  }
}

export function storeToken(token: string, method: 'oidc' | 'api_key'): void {
  localStorage.setItem(TOKEN_KEY, token)
  localStorage.setItem(AUTH_METHOD_KEY, method)
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
  localStorage.removeItem(AUTH_METHOD_KEY)
  localStorage.removeItem(OIDC_STATE_KEY)
  localStorage.removeItem(OIDC_VERIFIER_KEY)
}

export function storeOIDCState(state: string, verifier: string): void {
  localStorage.setItem(OIDC_STATE_KEY, state)
  localStorage.setItem(OIDC_VERIFIER_KEY, verifier)
}

export function getOIDCState(): { state: string | null; verifier: string | null } {
  return {
    state: localStorage.getItem(OIDC_STATE_KEY),
    verifier: localStorage.getItem(OIDC_VERIFIER_KEY),
  }
}
