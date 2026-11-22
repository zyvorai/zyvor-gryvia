// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// One status vocabulary for every page: a word has one meaning and one colour everywhere.
// Colour is deviation: healthy/neutral states are not green-washed, real problems are red.

export type Tone = 'ok' | 'warn' | 'bad' | 'info' | ''
export type PhaseGroup = 'pending' | 'running' | 'completed' | 'failed' | 'other'

const norm = (phase?: string) => (phase ?? '').trim().toLowerCase()

const TONES: Record<string, Tone> = {
  // finished well / healthy
  succeeded: 'ok',
  completed: 'ok',
  ready: 'ok',
  active: 'ok',
  enforced: 'ok',
  healthy: 'ok',
  // in progress
  running: 'info',
  idle: 'info',
  // waiting or changing: needs patience, not alarm
  pending: 'warn',
  queued: 'warn',
  scheduling: 'warn',
  initializing: 'warn',
  provisioning: 'warn',
  creating: 'warn',
  deploying: 'warn',
  syncing: 'warn',
  ingesting: 'warn',
  rollingback: 'warn',
  terminating: 'warn',
  degraded: 'warn',
  // broken
  failed: 'bad',
  error: 'bad',
  unhealthy: 'bad',
}

const GROUPS: Record<string, PhaseGroup> = {
  pending: 'pending',
  queued: 'pending',
  scheduling: 'pending',
  initializing: 'pending',
  provisioning: 'pending',
  creating: 'pending',
  deploying: 'pending',
  running: 'running',
  succeeded: 'completed',
  completed: 'completed',
  failed: 'failed',
  error: 'failed',
}

/** Pill/dot tone for a phase string ('' = neutral, e.g. Paused, Unknown, unrecognised). */
export function phaseTone(phase?: string): Tone {
  return TONES[norm(phase)] ?? ''
}

/** Coarse bucket used for counting: Succeeded and Completed are the same thing, Scheduling is pending. */
export function phaseGroup(phase?: string): PhaseGroup {
  return GROUPS[norm(phase)] ?? 'other'
}

/** Count items per group. */
export function countByGroup<T>(items: T[] | undefined, phaseOf: (item: T) => string | undefined): Record<PhaseGroup, number> {
  const out: Record<PhaseGroup, number> = { pending: 0, running: 0, completed: 0, failed: 0, other: 0 }
  for (const item of items ?? []) out[phaseGroup(phaseOf(item))] += 1
  return out
}
