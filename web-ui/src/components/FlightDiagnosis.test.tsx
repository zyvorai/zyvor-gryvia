// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { AxiosError } from 'axios'
import { api, type FlightDiagnosis as Diagnosis } from '@/lib/api'
import FlightDiagnosis from './FlightDiagnosis'

vi.mock('@/lib/api', () => ({ api: { getFlightDiagnosis: vi.fn() } }))
afterEach(() => {
  cleanup()
  vi.resetAllMocks()
})

function mount() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <FlightDiagnosis namespace="tenant-alpha" job="train" />
    </QueryClientProvider>,
  )
}

const diagnosis = (over: Partial<Diagnosis> = {}): Diagnosis => ({
  namespace: 'tenant-alpha',
  job: 'train',
  summary: 'no bottleneck detected in measured signals',
  partial: false,
  coverage: { total: 1, reachable: 1, reporting: 1, complete: true },
  nodes: ['n1'],
  findings: [],
  unavailable: [],
  measured: { n1: ['cgroup.cpu.stat'] },
  measurementCompleteness: {
    probesAttached: 5,
    probesSkipped: [],
    droppedEvents: { total: 0 },
    sampling: { ratio: 1 },
    nodesExpected: 1,
    nodesReporting: 1,
    missingNodes: [],
    complete: true,
    reasons: [],
  },
  ...over,
})

const load = () => userEvent.click(screen.getByRole('button', { name: 'Load diagnosis' }))

describe('Flight diagnosis', () => {
  it('loads on demand and shows findings with evidence and what was not measured', async () => {
    vi.mocked(api.getFlightDiagnosis).mockResolvedValue(
      diagnosis({
        partial: true,
        summary: '1 finding(s); most significant: cpu (critical, high confidence); PARTIAL VIEW',
        findings: [
          {
            kind: 'cpu',
            severity: 'critical',
            confidence: 'high',
            nodes: ['n1'],
            summary: 'CFS quota throttled 75% of periods',
            evidence: [{ node: 'n1', source: 'cgroup', metric: 'cpu_throttle_ratio', value: 0.75, window: '5m0s' }],
            whatWasNotMeasured: ['host-wide CPU contention is not read'],
          },
        ],
        unavailable: [{ node: 'n1', signal: 'cgroup.io.pressure', reason: 'PSI disabled' }],
        measurementCompleteness: { ...diagnosis().measurementCompleteness, nodesExpected: 3, missingNodes: ['n2', 'n3'], complete: false, reasons: ['no diagnosis from node(s): n2, n3'] },
      }),
    )
    mount()
    expect(api.getFlightDiagnosis).not.toHaveBeenCalled()
    await load()
    expect(await screen.findByText('Incomplete measurement')).toBeInTheDocument()
    expect(api.getFlightDiagnosis).toHaveBeenCalledWith('train', 'tenant-alpha')
    expect(screen.getByText('CFS quota throttled 75% of periods')).toBeInTheDocument()
    expect(screen.getByText(/n1: cgroup cpu_throttle_ratio = 0.75/)).toBeInTheDocument()
    expect(screen.getByText('host-wide CPU contention is not read')).toBeInTheDocument()
    expect(screen.getByText('n1: cgroup.io.pressure: PSI disabled')).toBeInTheDocument()
    expect(screen.getByText(/1 of 3 expected/)).toBeInTheDocument()
    expect(screen.getByText('Missing nodes: n2, n3')).toBeInTheDocument()
    expect(screen.queryByText(/Every expected node reported/)).not.toBeInTheDocument()
  })

  it('never shows the complete badge when expected nodes are unknown', async () => {
    vi.mocked(api.getFlightDiagnosis).mockResolvedValue(
      diagnosis({ partial: true, measurementCompleteness: { ...diagnosis().measurementCompleteness, nodesExpected: 'unknown', complete: false, reasons: ['expected nodes unknown'] } }),
    )
    mount()
    await load()
    expect(await screen.findByText('Incomplete measurement')).toBeInTheDocument()
    expect(screen.getByText(/1 of unknown expected/)).toBeInTheDocument()
  })

  it('shows the complete badge only when the gateway says so', async () => {
    vi.mocked(api.getFlightDiagnosis).mockResolvedValue(diagnosis())
    mount()
    await load()
    expect(await screen.findByText('Every expected node reported, nothing unavailable')).toBeInTheDocument()
    expect(screen.getByText(/no bottleneck detected in measured signals/)).toBeInTheDocument()
  })

  it('does not call an unreachable-collector failure healthy', async () => {
    vi.mocked(api.getFlightDiagnosis).mockRejectedValue(
      new AxiosError('failed', '503', undefined, undefined, { status: 503, data: { detail: 'No Flight Recorder collector is reachable' }, statusText: '', headers: {}, config: {} as never }),
    )
    mount()
    await load()
    expect(await screen.findByText(/this does not mean the job is healthy/)).toBeInTheDocument()
  })
})
