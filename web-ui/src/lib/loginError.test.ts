// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest'
import { loginErrorMessage } from './auth'

const http = (status: number) => ({ response: { status } })

describe('loginErrorMessage', () => {
  it('says the credentials are wrong for 401, 403 and 422', () => {
    for (const s of [401, 403, 422]) expect(loginErrorMessage(http(s))).toBe('Wrong username or password.')
  })
  it('explains rate limiting', () => {
    expect(loginErrorMessage(http(429))).toMatch(/Too many sign-in attempts/)
  })
  it('reports an unreachable gateway when there is no response', () => {
    expect(loginErrorMessage(new Error('Network Error'))).toMatch(/Could not reach the API gateway/)
    expect(loginErrorMessage(http(502))).toMatch(/Could not reach/)
  })
})
