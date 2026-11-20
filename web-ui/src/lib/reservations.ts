// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Pure helpers for the Reservations page. A reservation taints and labels whole nodes for its
// owner (see docs/gpuaas-completion.md); jobs opt in with the gryvia.io/reservation annotation.
import { intError, nameError, parseIntStrict } from './forms'

export type ReservationState = 'pending' | 'active' | 'expired' | 'cancelled'

export interface Reservation {
  name: string
  owner?: { type: string; name: string }
  gpuType?: string
  gpuCount?: number
  schedule?: { type?: string; startTime?: string; endTime?: string; recurrence?: { cron?: string; duration?: string } | null }
  exclusive?: boolean
  state?: ReservationState | string
  allocatedNodes?: string[]
  allocatedGPUs?: number
  actualStartTime?: string
  actualEndTime?: string
}

export interface ReservationBody {
  name?: string
  gpuType: string
  gpuCount: number
  scheduleType: 'immediate' | 'scheduled'
  startTime?: string
  endTime?: string
  exclusive: boolean
}

export interface ReservationForm {
  name: string
  gpuType: string
  gpuCount: string
  startTime: string // datetime-local value, blank = start now
  endTime: string // datetime-local value
  exclusive: boolean
}

export const EMPTY_RESERVATION_FORM: ReservationForm = { name: '', gpuType: '', gpuCount: '8', startTime: '', endTime: '', exclusive: true }

/** Only pending and active reservations can be cancelled. */
export function canCancel(r: Pick<Reservation, 'state'>): boolean {
  return r.state === 'pending' || r.state === 'active'
}

export function stateTone(state?: string): 'ok' | 'warn' | 'bad' | '' {
  switch (state) {
    case 'active':
      return 'ok'
    case 'pending':
      return 'warn'
    case 'cancelled':
      return 'bad'
    default:
      return ''
  }
}

export function ownerLabel(r: Pick<Reservation, 'owner'>): string {
  return r.owner?.name ? `${r.owner.type ?? 'owner'}/${r.owner.name}` : '—'
}

export function reservationErrors(f: ReservationForm, now: number = Date.now()): Partial<Record<keyof ReservationForm, string>> {
  const e: Partial<Record<keyof ReservationForm, string>> = {}
  if (f.name.trim() !== '') {
    const n = nameError(f.name.trim())
    if (n) e.name = n
  }
  if (f.gpuType.trim() === '') e.gpuType = 'Enter a GPU type.'
  const c = intError(f.gpuCount, 1, 4096)
  if (c) e.gpuCount = c
  const end = Date.parse(f.endTime)
  if (f.endTime === '' || Number.isNaN(end)) e.endTime = 'Choose when the reservation ends.'
  else if (end <= now) e.endTime = 'The end must be in the future.'
  if (f.startTime !== '') {
    const start = Date.parse(f.startTime)
    if (Number.isNaN(start)) e.startTime = 'Enter a valid start.'
    else if (!e.endTime && start >= end) e.startTime = 'The start must be before the end.'
  }
  return e
}

export function buildReservationBody(f: ReservationForm): ReservationBody {
  const body: ReservationBody = {
    gpuType: f.gpuType.trim(),
    gpuCount: parseIntStrict(f.gpuCount) ?? 0,
    scheduleType: f.startTime ? 'scheduled' : 'immediate',
    endTime: new Date(f.endTime).toISOString(),
    exclusive: f.exclusive,
  }
  if (f.name.trim()) body.name = f.name.trim()
  if (f.startTime) body.startTime = new Date(f.startTime).toISOString()
  return body
}
