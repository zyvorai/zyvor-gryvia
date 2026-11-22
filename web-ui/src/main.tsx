// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import './index.css'
import { clearReloadGuard, reloadOnce } from './lib/chunkError'

// A redeploy replaces hashed chunks; recover open tabs by reloading once.
window.addEventListener('vite:preloadError', (event) => {
  if (reloadOnce()) event.preventDefault()
})
window.addEventListener('load', () => window.setTimeout(clearReloadGuard, 10_000))

const rootEl = document.getElementById('root')
if (!rootEl) throw new Error('Root element #root not found')

ReactDOM.createRoot(rootEl).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
