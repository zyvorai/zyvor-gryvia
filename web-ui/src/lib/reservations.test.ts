// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest'
import { EMPTY_RESERVATION_FORM, buildReservationBody, canCancel, ownerLabel, reservationErrors, stateTone } from './reservations'

const NOW = Date.parse('2030-01-01T00:00:00Z')
const ok = { ...EMPTY_RESERVATION_FORM, gpuType: 'H100', gpuCount: '8', endTime: '2030-01-02T00:00' }

describe('reservationErrors', () => {
  it('accepts a valid form', () => expect(reservationErrors(ok, NOW)).toEqual({}))
  it('flags missing type, bad count, past or missing end', () => {
    expect(reservationErrors({ ...ok, gpuType: ' ' }, NOW).gpuType).toBeTruthy()
    expect(reservationErrors({ ...ok, gpuCount: '0' }, NOW).gpuCount).toBeTruthy()
    expect(reservationErrors({ ...ok, gpuCount: '1.5' }, NOW).gpuCount).toBeTruthy()
    expect(reservationErrors({ ...ok, endTime: '' }, NOW).endTime).toBeTruthy()
    expect(reservationErrors({ ...ok, endTime: '2029-01-01T00:00' }, NOW).endTime).toBeTruthy()
  })
  it('requires start before end and a valid name', () => {
    expect(reservationErrors({ ...ok, startTime: '2030-01-03T00:00' }, NOW).startTime).toBeTruthy()
    expect(reservationErrors({ ...ok, name: 'Bad Name' }, NOW).name).toBeTruthy()
  })
})

describe('buildReservationBody', () => {
  it('is immediate without a start and scheduled with one', () => {
    const a = buildReservationBody(ok)
    expect(a.scheduleType).toBe('immediate')
    expect(a.startTime).toBeUndefined()
    expect(a.name).toBeUndefined()
    expect(a.gpuCount).toBe(8)
    const b = buildReservationBody({ ...ok, startTime: '2030-01-01T10:00', name: 'r1', exclusive: false })
    expect(b.scheduleType).toBe('scheduled')
    expect(b.name).toBe('r1')
    expect(b.exclusive).toBe(false)
    expect(b.endTime).toMatch(/Z$/)
  })
})

describe('helpers', () => {
  it('cancel only pending or active', () => {
    expect(canCancel({ state: 'active' })).toBe(true)
    expect(canCancel({ state: 'pending' })).toBe(true)
    expect(canCancel({ state: 'expired' })).toBe(false)
    expect(canCancel({ state: 'cancelled' })).toBe(false)
  })
  it('tones and owner label', () => {
    expect(stateTone('active')).toBe('ok')
    expect(stateTone('expired')).toBe('')
    expect(ownerLabel({ owner: { type: 'team', name: 'ml' } })).toBe('team/ml')
    expect(ownerLabel({})).toBe('—')
  })
})
