// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { Component, type ErrorInfo, type ReactNode } from 'react'
import { isChunkLoadError, reloadOnce } from '@/lib/chunkError'

interface Props {
  children: ReactNode
}

interface State {
  error: Error | null
  copied: boolean
}

/** Catches a crashing page without taking the navigation down; resets when the route changes (via `key`). */
export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null, copied: false }

  static getDerivedStateFromError(error: Error): State {
    return { error, copied: false }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('Page crashed:', error, info.componentStack)
    if (isChunkLoadError(error)) reloadOnce()
  }

  copyDetails = async () => {
    const { error } = this.state
    const text = `${error?.name}: ${error?.message}\n${error?.stack ?? ''}\n${window.location.href}`
    try {
      await navigator.clipboard.writeText(text)
      this.setState({ copied: true })
    } catch {
      /* clipboard unavailable */
    }
  }

  render() {
    const { error, copied } = this.state
    if (!error) return this.props.children
    const stale = isChunkLoadError(error)
    return (
      <div className="grid">
        <section className="card span3" role="alert">
          <p className="eyebrow">ERROR</p>
          <h2 className="card-title">{stale ? 'Gryvia was updated.' : 'This page hit a problem.'}</h2>
          <p>
            {stale
              ? 'A newer version was deployed while this tab was open. Reload to continue.'
              : `${error.message || 'An unexpected error occurred.'} The rest of the app still works.`}
          </p>
          <div className="toolbar">
            {stale ? (
              <button type="button" className="primary" onClick={() => window.location.reload()}>
                Reload
              </button>
            ) : (
              <button type="button" className="primary" onClick={() => this.setState({ error: null, copied: false })}>
                Try again
              </button>
            )}
            <button type="button" className="btn-secondary" onClick={this.copyDetails}>
              {copied ? 'Copied' : 'Copy details'}
            </button>
          </div>
        </section>
      </div>
    )
  }
}
