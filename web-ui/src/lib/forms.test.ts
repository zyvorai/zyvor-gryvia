// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { buildIdleTimeout, buildStorage, fieldAria, intError, nameError, parseIntStrict } from './forms'

describe('buildStorage', () => {
  it('builds valid quantities', () => {
    expect(buildStorage('50', 'Gi')).toBe('50Gi')
    expect(buildStorage(' 2 ', 'Ti')).toBe('2Ti')
    expect(buildStorage('999999', 'Mi')).toBe('999999Mi')
  })
  it('rejects invalid ones', () => {
    expect(buildStorage('0', 'Gi')).toBeNull()
    expect(buildStorage('', 'Gi')).toBeNull()
    expect(buildStorage('1.5', 'Gi')).toBeNull()
    expect(buildStorage('1000000', 'Gi')).toBeNull()
    expect(buildStorage('5', 'GB')).toBeNull()
  })
})

describe('buildIdleTimeout', () => {
  it('builds and rejects', () => {
    expect(buildIdleTimeout('30', 'm')).toBe('30m')
    expect(buildIdleTimeout('2', 'h')).toBe('2h')
    expect(buildIdleTimeout('0', 'm')).toBeNull()
    expect(buildIdleTimeout('100000', 'm')).toBeNull()
    expect(buildIdleTimeout('30', 's')).toBeNull()
  })
})

describe('nameError / intError', () => {
  it('validates names', () => {
    expect(nameError('my-ws')).toBeNull()
    expect(nameError('')).not.toBeNull()
    expect(nameError('My_WS')).not.toBeNull()
    expect(nameError('-a')).not.toBeNull()
    expect(nameError('a'.repeat(64))).not.toBeNull()
  })
  it('validates integers strictly', () => {
    expect(parseIntStrict('12')).toBe(12)
    expect(parseIntStrict('1e2')).toBeNull()
    expect(parseIntStrict('')).toBeNull()
    expect(intError('64', 1, 64)).toBeNull()
    expect(intError('65', 1, 64)).not.toBeNull()
    expect(intError('abc', 1, 64)).not.toBeNull()
  })
})

describe('fieldAria', () => {
  it('marks invalid and describes by the message id only when there is something to read', () => {
    expect(fieldAria('f', 'Bad')).toEqual({ 'aria-invalid': true, 'aria-describedby': 'f-msg' })
    expect(fieldAria('f', null, true)).toEqual({ 'aria-invalid': false, 'aria-describedby': 'f-msg' })
    expect(fieldAria('f', null)).toEqual({ 'aria-invalid': false, 'aria-describedby': undefined })
  })
})
