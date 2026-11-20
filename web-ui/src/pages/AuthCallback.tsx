// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useEffect, useRef } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { useAuth } from '@/lib/auth'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'

const MAX_ERROR_LEN = 200

export default function AuthCallback() {
  useDocumentTitle('Signing in')
  const { handleOIDCCallback, isAuthenticated, error: authError } = useAuth()
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const processed = useRef(false)

  useEffect(() => {
    if (processed.current) return

    const code = searchParams.get('code')
    const errorParam = searchParams.get('error')

    if (errorParam) {
      // Stay on this page and show the error with a way back; no timed redirect.
      processed.current = true
      return
    }

    if (code) {
      processed.current = true
      handleOIDCCallback(code)
    } else {
      // No code and no error - nothing to complete, go to login
      navigate('/login', { replace: true })
    }
  }, [searchParams, handleOIDCCallback, navigate])

  // Redirect to dashboard once authenticated
  useEffect(() => {
    if (isAuthenticated) {
      navigate('/dashboard', { replace: true })
    }
  }, [isAuthenticated, navigate])

  // The query string is attacker-controlled: it is only ever rendered as text, and truncated.
  const rawUrlError = searchParams.get('error_description') || searchParams.get('error')
  const urlError = rawUrlError && rawUrlError.length > MAX_ERROR_LEN ? `${rawUrlError.slice(0, MAX_ERROR_LEN)}…` : rawUrlError
  const displayError = urlError || authError

  return (
    <div className="login-shell">
      {displayError ? (
        <div className="stack">
          <h1 className="apple-display">Authentication failed.</h1>
          <p className="warning" role="alert">{displayError}</p>
          <p>
            <Link to="/login" replace className="buttonlike primary">
              Back to sign in
            </Link>
          </p>
        </div>
      ) : (
        <div className="stack">
          <h1 className="apple-display">Completing sign in…</h1>
          <div className="spinner" role="status" aria-label="Completing sign in" />
        </div>
      )}
    </div>
  )
}
