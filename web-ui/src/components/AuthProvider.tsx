// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState, useEffect, useCallback, type ReactNode } from 'react'
import {
  AuthContext,
  type AuthConfig,
  type UserInfo,
  fetchAuthConfig,
  fetchUserInfo,
  loginWithCredentials,
  loginErrorMessage,
  exchangeCodeForToken,
  buildAuthorizationURLWithPKCE,
  storeToken,
  clearToken,
  storeOIDCState,
  getOIDCState,
  getStoredToken,
} from '@/lib/auth'

interface AuthProviderProps {
  children: ReactNode
}

export default function AuthProvider({ children }: AuthProviderProps) {
  const [isAuthenticated, setIsAuthenticated] = useState(false)
  const [isLoading, setIsLoading] = useState(() => getStoredToken() !== null)
  const [user, setUser] = useState<UserInfo | null>(null)
  const [authConfig, setAuthConfig] = useState<AuthConfig | null>(null)
  const [error, setError] = useState<string | null>(null)

  // Load auth config on mount
  useEffect(() => {
    fetchAuthConfig()
      .then((config) => setAuthConfig(config))
      .catch(() => {
        // If config endpoint is unreachable, default to API key only
        setAuthConfig({ oidcEnabled: false, apiKeyEnabled: true })
      })
  }, [])

  // Validate existing token on mount
  useEffect(() => {
    const token = getStoredToken()
    if (!token) return

    fetchUserInfo(token)
      .then((userInfo) => {
        setUser(userInfo)
        setIsAuthenticated(true)
      })
      .catch(() => {
        // Token is invalid or expired
        clearToken()
      })
      .finally(() => setIsLoading(false))
  }, [])

  // Both sign-in forms exchange their secret for a short-lived session token at the gateway; the
  // API key itself is never kept in the browser.
  const signIn = useCallback(async (username: string, secret: string) => {
    setError(null)
    if (username.trim() === '' || secret === '') {
      const message = 'Wrong username or password.'
      setError(message)
      throw new Error(message)
    }
    try {
      const sessionToken = await loginWithCredentials(username.trim(), secret)
      const userInfo = await fetchUserInfo(sessionToken)
      storeToken(sessionToken, 'api_key')
      setUser(userInfo)
      setIsAuthenticated(true)
    } catch (err) {
      const message = loginErrorMessage(err)
      setError(message)
      throw new Error(message, { cause: err })
    }
  }, [])

  const loginWithApiKey = useCallback((apiKey: string) => signIn('admin', apiKey), [signIn])

  const loginWithPassword = useCallback(
    (username: string, password: string) => signIn(username, password),
    [signIn],
  )

  const loginWithSSO = useCallback(async () => {
    if (!authConfig?.oidcEnabled || !authConfig.authorizationEndpoint) {
      setError('SSO is not configured')
      return
    }

    try {
      const { url, state, verifier } = await buildAuthorizationURLWithPKCE(authConfig)
      storeOIDCState(state, verifier)
      window.location.href = url
    } catch {
      setError('Failed to initiate SSO login')
    }
  }, [authConfig])

  const handleOIDCCallback = useCallback(
    async (code: string) => {
      setError(null)
      setIsLoading(true)

      const { state: storedState, verifier } = getOIDCState()
      const urlState = new URLSearchParams(window.location.search).get('state')

      if (!storedState || storedState !== urlState) {
        setError('Invalid OAuth state. Please try logging in again.')
        setIsLoading(false)
        return
      }

      if (!verifier || !authConfig?.tokenEndpoint || !authConfig?.clientId) {
        setError('Missing OIDC configuration for token exchange')
        setIsLoading(false)
        return
      }

      try {
        const token = await exchangeCodeForToken(
          code,
          authConfig.tokenEndpoint,
          authConfig.clientId,
          verifier,
        )

        storeToken(token, 'oidc')
        const userInfo = await fetchUserInfo(token)
        setUser(userInfo)
        setIsAuthenticated(true)
      } catch {
        setError('Failed to complete SSO login. Please try again.')
        clearToken()
      } finally {
        setIsLoading(false)
      }
    },
    [authConfig],
  )

  const logout = useCallback(() => {
    clearToken()
    setUser(null)
    setIsAuthenticated(false)
    setError(null)
  }, [])

  return (
    <AuthContext.Provider
      value={{
        isAuthenticated,
        isLoading,
        user,
        authConfig,
        loginWithApiKey,
        loginWithPassword,
        loginWithSSO,
        handleOIDCCallback,
        logout,
        error,
      }}
    >
      {children}
    </AuthContext.Provider>
  )
}
