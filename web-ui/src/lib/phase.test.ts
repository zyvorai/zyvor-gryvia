// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { countByGroup, phaseGroup, phaseTone } from './phase'

describe('phaseTone', () => {
  it('is case-insensitive and covers the operator phases', () => {
    expect(phaseTone('Succeeded')).toBe('ok')
    expect(phaseTone('completed')).toBe('ok')
    expect(phaseTone('Scheduling')).toBe('warn')
    expect(phaseTone('Terminating')).toBe('warn')
    expect(phaseTone('RollingBack')).toBe('warn')
    expect(phaseTone('Failed')).toBe('bad')
    expect(phaseTone('Running')).toBe('info')
  })

  it('leaves unknown and paused neutral', () => {
    expect(phaseTone('Unknown')).toBe('')
    expect(phaseTone('Paused')).toBe('')
    expect(phaseTone(undefined)).toBe('')
  })
})

describe('phaseGroup', () => {
  it('treats Succeeded and Completed alike and Scheduling as pending', () => {
    expect(phaseGroup('Succeeded')).toBe('completed')
    expect(phaseGroup('Completed')).toBe('completed')
    expect(phaseGroup('Scheduling')).toBe('pending')
    expect(phaseGroup('Queued')).toBe('pending')
  })

  it('counts a mixed list', () => {
    const jobs = [{ p: 'Running' }, { p: 'Succeeded' }, { p: 'Scheduling' }, { p: 'Failed' }, { p: undefined }]
    expect(countByGroup(jobs, (j) => j.p)).toEqual({ pending: 1, running: 1, completed: 1, failed: 1, other: 1 })
  })
})
