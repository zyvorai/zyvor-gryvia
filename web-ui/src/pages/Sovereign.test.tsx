// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { api } from '@/lib/api'
import Sovereign from './Sovereign'

vi.mock('@/lib/api', () => ({ api: { getSovereign: vi.fn(), getClusterStats: vi.fn() } }))
afterEach(() => {
  cleanup()
  vi.resetAllMocks()
})

function mount() {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter>
        <Sovereign />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('Sovereign AI OS page', () => {
  it('shows Zyntra healthy with its counts and console links, and Netra not installed', async () => {
    vi.mocked(api.getClusterStats).mockResolvedValue({ totalGPUs: 8, availableGPUs: 3, allocatedGPUs: 5, utilizationPercent: 60, totalJobs: 4, runningJobs: 2, pendingJobs: 1, completedJobs: 1, failedJobs: 0, totalNodes: 1 })
    vi.mocked(api.getSovereign).mockResolvedValue({
      enabled: true,
      products: [
        { name: 'Zyntra', role: '', installed: true, state: 'healthy', console: 'https://zyntra.example', version: '0.4.0', executeMode: 'apply', pack: 'sovereign-aios', sources: { total: 6, healthy: 6 }, openGaps: 2, pendingProposals: 1 },
        { name: 'Netra', role: '', installed: false, state: 'not installed', console: null },
      ],
    })
    mount()
    expect(await screen.findByText('v0.4.0 · applies approved changes · 6/6 sources healthy')).toBeInTheDocument()
    expect(screen.getByRole('rowheader', { name: 'Proposals waiting for approval' }).parentElement).toHaveTextContent('1')
    expect(screen.getByRole('link', { name: 'Approvals' })).toHaveAttribute('href', 'https://zyntra.example/#/approvals')
    expect(screen.getByText('not installed')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Open Netra' })).toBeNull()
    expect(await screen.findByText(/3 of 8 GPUs free/)).toBeInTheDocument()
  })

  it('says why Zyntra is unreachable', async () => {
    vi.mocked(api.getClusterStats).mockRejectedValue(new Error('down'))
    vi.mocked(api.getSovereign).mockResolvedValue({
      enabled: true,
      products: [
        { name: 'Zyntra', role: '', installed: true, state: 'unreachable', error: 'connection refused' },
        { name: 'Netra', role: '', installed: true, state: 'healthy', console: 'https://netra.example' },
      ],
    })
    mount()
    expect(await screen.findByText('connection refused')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Open Netra' })).toHaveAttribute('href', 'https://netra.example')
  })

  it('offers a retry when the gateway fails', async () => {
    vi.mocked(api.getClusterStats).mockRejectedValue(new Error('down'))
    vi.mocked(api.getSovereign).mockRejectedValue(new Error('boom'))
    mount()
    expect(await screen.findByRole('button', { name: 'Retry' })).toBeInTheDocument()
  })
})
