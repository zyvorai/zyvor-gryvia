// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

const RELOAD_KEY = 'gryvia_chunk_reload'

/** True for the errors a browser raises when a lazily loaded chunk no longer exists (after a redeploy). */
export function isChunkLoadError(err: unknown): boolean {
  const msg = err instanceof Error ? `${err.name} ${err.message}` : String(err ?? '')
  return /Failed to fetch dynamically imported module|error loading dynamically imported module|Importing a module script failed|ChunkLoadError|Loading chunk [\w-]+ failed/i.test(msg)
}

/**
 * Reloads the page once so it picks up the new build. Returns false when a reload was already tried
 * for this tab (so a genuinely missing file does not loop forever).
 */
export function reloadOnce(): boolean {
  try {
    if (sessionStorage.getItem(RELOAD_KEY)) return false
    sessionStorage.setItem(RELOAD_KEY, String(Date.now()))
  } catch {
    return false
  }
  window.location.reload()
  return true
}

/** Clears the guard after a successful load so the next deploy can recover again. */
export function clearReloadGuard(): void {
  try {
    sessionStorage.removeItem(RELOAD_KEY)
  } catch {
    /* storage unavailable */
  }
}
