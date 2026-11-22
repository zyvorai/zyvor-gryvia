// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { api, type InferenceLatency } from '@/lib/api'
import InferencePanel from './InferencePanel'

vi.mock('@/lib/api', () => ({ api: { getInferenceLatency: vi.fn() } }))
afterEach(() => {
  cleanup()
  vi.resetAllMocks()
})

function mount() {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <InferencePanel namespace="ml" job="serve" />
    </QueryClientProvider>,
  )
}

const data = (over: Partial<InferenceLatency> = {}): InferenceLatency => ({ namespace: 'ml', job: 'serve', available: true, engine: 'vllm', ...over })

describe('Inference panel', () => {
  it('renders nothing when no engine metrics were published', async () => {
    vi.mocked(api.getInferenceLatency).mockResolvedValue(data({ available: false, engine: undefined }))
    const { container } = mount()
    await waitFor(() => expect(api.getInferenceLatency).toHaveBeenCalledWith('serve', 'ml'))
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when the request fails', async () => {
    vi.mocked(api.getInferenceLatency).mockRejectedValue(new Error('boom'))
    const { container } = mount()
    await waitFor(() => expect(api.getInferenceLatency).toHaveBeenCalled())
    expect(container).toBeEmptyDOMElement()
  })

  it('shows TTFT, ITL and queue p99 with the engine name', async () => {
    vi.mocked(api.getInferenceLatency).mockResolvedValue(
      data({ ttftP99ms: 475, itlP99ms: 72.2, queueTimeP99ms: 50, e2eP99ms: 2250, requestsWaiting: 3, kvCacheUsage: 0.42, inferWaitP99ms: 4 }),
    )
    mount()
    expect(await screen.findByRole('heading', { name: 'Inference latency' })).toBeInTheDocument()
    expect(screen.getByText('vLLM')).toBeInTheDocument()
    const cell = (label: string) => screen.getByRole('rowheader', { name: label }).parentElement!
    expect(cell('Time to first token, p99')).toHaveTextContent('475 ms')
    expect(cell('Inter-token latency, p99')).toHaveTextContent('72.2 ms')
    expect(cell('Engine queue time, p99')).toHaveTextContent('50 ms')
    expect(cell('End-to-end request latency, p99')).toHaveTextContent('2.25 s')
    expect(cell('Requests waiting')).toHaveTextContent('3')
    expect(cell('KV cache usage')).toHaveTextContent('42%')
    expect(cell('Network accept wait, p99')).toHaveTextContent('4 ms')
    expect(cell('Network accept wait, p99')).toHaveTextContent(/not queue time/)
  })

  it('shows "not measured" for what the engine does not export', async () => {
    vi.mocked(api.getInferenceLatency).mockResolvedValue(data({ engine: 'triton', queueTimeMeanMs: 0.86, e2eMeanMs: 7.4, requestsWaiting: 6 }))
    mount()
    await screen.findByRole('heading', { name: 'Inference latency' })
    const cell = (label: string) => screen.getByRole('rowheader', { name: label }).parentElement!
    expect(cell('Time to first token, p99')).toHaveTextContent('not measured')
    expect(cell('Inter-token latency, p99')).toHaveTextContent('not measured')
    expect(cell('KV cache usage')).toHaveTextContent('not measured')
    expect(cell('Network accept wait, p99')).toHaveTextContent('not measured')
    // Counter-only engine: a mean, labelled as a mean, next to the empty p99 row.
    expect(cell('Engine queue time, mean')).toHaveTextContent('0.86 ms')
    expect(cell('Engine queue time, p99')).toHaveTextContent('not measured')
    expect(screen.getByText('NVIDIA Triton')).toBeInTheDocument()
  })

  it('never renders an engine name it does not know as markup', async () => {
    vi.mocked(api.getInferenceLatency).mockResolvedValue(data({ engine: '<img src=x onerror=alert(1)>' }))
    mount()
    await screen.findByRole('heading', { name: 'Inference latency' })
    expect(document.querySelector('img')).toBeNull()
    expect(screen.getByText('Unknown engine')).toBeInTheDocument()
  })
})
