// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { AxiosError } from 'axios'
import { api, type FlightReport } from '@/lib/api'
import FlightRecorder from './FlightRecorder'

vi.mock('@/lib/api', () => ({ api: { getFlightReport: vi.fn() } }))
afterEach(() => {
  cleanup()
  vi.resetAllMocks()
})

function mount() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <FlightRecorder namespace="tenant-alpha" job="train" />
    </QueryClientProvider>,
  )
}

const report = (over: Partial<FlightReport> = {}): FlightReport => ({
  namespace: 'tenant-alpha',
  job: 'train',
  coverage: { total: 3, reachable: 2, reporting: 1, complete: false },
  truncated: false,
  nodes: ['n1'],
  counts: {},
  findings: [],
  events: [],
  ...over,
})

function httpError(status: number, detail: string) {
  return new AxiosError('failed', String(status), undefined, undefined, {
    status,
    data: { detail },
    statusText: '',
    headers: {},
    config: {} as never,
  })
}

const load = () => userEvent.click(screen.getByRole('button', { name: 'Load observations' }))

describe('Flight Recorder', () => {
  it('loads on demand in the job namespace and shows partial coverage', async () => {
    vi.mocked(api.getFlightReport).mockResolvedValue(report())
    mount()
    expect(api.getFlightReport).not.toHaveBeenCalled()
    await load()
    expect(await screen.findByText('Partial coverage')).toBeInTheDocument()
    expect(api.getFlightReport).toHaveBeenCalledWith('train', 'tenant-alpha')
    expect(screen.getByText(/2 of 3 discovered collectors reachable/)).toBeInTheDocument()
    expect(screen.getByText('No findings in the retained sample.')).toBeInTheDocument()
    expect(screen.queryByText('All discovered collectors answered')).not.toBeInTheDocument()
  })

  it('marks a truncated report partial even when every collector answered', async () => {
    vi.mocked(api.getFlightReport).mockResolvedValue(report({ coverage: { total: 1, reachable: 1, reporting: 1, complete: true }, truncated: true }))
    mount()
    await load()
    expect(await screen.findByText('Partial coverage')).toBeInTheDocument()
    expect(screen.getByText(/Events were dropped by a limit/)).toBeInTheDocument()
  })

  it('says complete only for a complete, untruncated report', async () => {
    vi.mocked(api.getFlightReport).mockResolvedValue(report({ coverage: { total: 1, reachable: 1, reporting: 1, complete: true } }))
    mount()
    await load()
    expect(await screen.findByText('All discovered collectors answered')).toBeInTheDocument()
    expect(screen.getByText(/Nodes without a running collector are not counted/)).toBeInTheDocument()
  })

  it('renders event and finding text as plain text', async () => {
    const evil = '<img src=x onerror=alert(1)>'
    vi.mocked(api.getFlightReport).mockResolvedValue(
      report({
        findings: [{ node: 'n1', code: 'c', evidence: evil }],
        events: [{ time: '2026-01-01T00:00:00Z', node: 'n1', kind: 'nccl_collective', operation: 'AllReduce', identity: { pod: evil } }],
      }),
    )
    mount()
    await load()
    expect((await screen.findAllByText(evil, { exact: false })).length).toBeGreaterThan(0)
    expect(document.querySelector('img')).toBeNull()
    expect(screen.getAllByText('Unknown').length).toBeGreaterThan(0)
  })

  it('explains a 403 for another namespace', async () => {
    vi.mocked(api.getFlightReport).mockRejectedValue(httpError(403, 'Namespace is not available to this user'))
    mount()
    await load()
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot read Flight Recorder data for namespace tenant-alpha')
  })

  it('shows a 503 as unavailable, never as healthy', async () => {
    vi.mocked(api.getFlightReport).mockRejectedValue(httpError(503, 'No Flight Recorder collector is reachable'))
    mount()
    await load()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('No Flight Recorder collector is reachable')
    expect(alert).toHaveTextContent('does not mean the job is healthy')
    expect(screen.queryByText('No findings in the retained sample.')).not.toBeInTheDocument()
  })
})
