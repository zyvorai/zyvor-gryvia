import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { api } from '@/lib/api'
import SubmitJob from './SubmitJob'

afterEach(() => { cleanup(); vi.restoreAllMocks() })

function mount() {
  vi.spyOn(api, 'getJobs').mockResolvedValue([])
  vi.spyOn(api, 'getNodes').mockResolvedValue([])
  vi.spyOn(api, 'getQuotas').mockResolvedValue([])
  vi.spyOn(api, 'getClusterStats').mockResolvedValue({} as Awaited<ReturnType<typeof api.getClusterStats>>)
  render(<MemoryRouter><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><SubmitJob /></QueryClientProvider></MemoryRouter>)
}

it('previews the manifest without creating a job and hides success after edits', async () => {
  const create = vi.spyOn(api, 'createJob')
  const preview = vi.spyOn(api, 'preflightJob').mockResolvedValue({ admitted: true, persisted: false, namespace: 'tenant-one', name: 'train', warnings: ['no node currently carries GPU type "H100"'], limitations: ['No GPU capacity is reserved'] })
  mount()
  const name = screen.getByPlaceholderText('my-training-job')
  await userEvent.type(name, 'train')
  await userEvent.click(screen.getByRole('button', { name: 'Check admission' }))
  await waitFor(() => expect(preview).toHaveBeenCalled())
  expect(await screen.findByText(/Kubernetes admission passed/)).toBeInTheDocument()
  expect(screen.getByText(/no node currently carries GPU type/)).toBeInTheDocument()
  expect(create).not.toHaveBeenCalled()
  await userEvent.type(name, '-edited')
  expect(screen.queryByText(/Kubernetes admission passed/)).not.toBeInTheDocument()
})

it('shows admission failures without submitting', async () => {
  const create = vi.spyOn(api, 'createJob')
  vi.spyOn(api, 'preflightJob').mockRejectedValue(new Error('Admission unavailable'))
  mount()
  await userEvent.type(screen.getByPlaceholderText('my-training-job'), 'train')
  await userEvent.click(screen.getByRole('button', { name: 'Check admission' }))
  expect(await screen.findByText(/Admission preview failed/)).toBeInTheDocument()
  expect(create).not.toHaveBeenCalled()
})
