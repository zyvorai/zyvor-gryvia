// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { clearReloadGuard, isChunkLoadError, reloadOnce } from './chunkError'

describe('isChunkLoadError', () => {
  it('recognises browser chunk failures', () => {
    expect(isChunkLoadError(new TypeError('Failed to fetch dynamically imported module: https://x/assets/Jobs-1.js'))).toBe(true)
    expect(isChunkLoadError(new Error('error loading dynamically imported module'))).toBe(true)
    expect(isChunkLoadError(new Error('Importing a module script failed.'))).toBe(true)
  })
  it('ignores ordinary errors', () => {
    expect(isChunkLoadError(new Error('boom'))).toBe(false)
    expect(isChunkLoadError(undefined)).toBe(false)
  })
})

describe('reloadOnce', () => {
  beforeEach(() => {
    sessionStorage.clear()
    vi.stubGlobal('location', { ...window.location, reload: vi.fn() })
  })
  it('reloads once per tab, then refuses until the guard is cleared', () => {
    expect(reloadOnce()).toBe(true)
    expect(window.location.reload).toHaveBeenCalledTimes(1)
    expect(reloadOnce()).toBe(false)
    expect(window.location.reload).toHaveBeenCalledTimes(1)
    clearReloadGuard()
    expect(reloadOnce()).toBe(true)
  })
})
