// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { AxiosError } from 'axios'
import { describe, expect, it } from 'vitest'
import { errorMessage } from './errors'

function axiosError(data: unknown): AxiosError {
  const err = new AxiosError('Request failed with status code 422')
  err.response = { data, status: 422, statusText: '', headers: {}, config: {} as never }
  return err
}

describe('errorMessage', () => {
  it('uses a string detail from the gateway', () => {
    expect(errorMessage(axiosError({ detail: "modelRef 'x' not found" }))).toBe("modelRef 'x' not found")
  })

  it('formats the first validation error with its location', () => {
    expect(errorMessage(axiosError({ detail: [{ loc: ['body', 'spec', 'name'], msg: 'too short' }] }))).toBe('spec.name: too short')
  })

  it('falls back to the error message', () => {
    expect(errorMessage(new Error('boom'))).toBe('boom')
    expect(errorMessage('weird')).toBe('Unknown error')
  })
})
