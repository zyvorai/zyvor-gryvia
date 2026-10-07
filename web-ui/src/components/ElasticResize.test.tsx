import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { api } from '@/lib/api'
import type { GryviaAIJob } from '@/types'
import ElasticResize from './ElasticResize'

vi.mock('@/lib/api', () => ({ api: { resizeJob: vi.fn() } }))
vi.mock('@/lib/notify', () => ({ notify: { success: vi.fn(), error: vi.fn() } }))
afterEach(() => {
  cleanup()
  vi.resetAllMocks()
})

function job(over: { phase?: string; labels?: Record<string, string>; elastic?: boolean; desired?: number; current?: number } = {}): GryviaAIJob {
  return {
    apiVersion: 'gryvia.io/v1alpha1',
    kind: 'GryviaAIJob',
    metadata: { name: 'el', labels: over.labels },
    spec: {
      type: 'training',
      image: 'img',
      gpus: 0,
      distributed: {
        enabled: true,
        nodes: 4,
        ...(over.elastic === false ? {} : { elastic: { minNodes: 2, desiredNodes: over.desired } }),
      },
    },
    status: { phase: (over.phase ?? 'Running') as 'Running', elastic: { currentNodes: over.current ?? 3 } },
  }
}

function mount(j: GryviaAIJob) {
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <ElasticResize job={j} />
    </QueryClientProvider>,
  )
}

describe('ElasticResize', () => {
  it('renders nothing for a non-elastic job', () => {
    const { container } = mount(job({ elastic: false }))
    expect(container).toBeEmptyDOMElement()
  })

  it('resizes within the bounds', async () => {
    vi.mocked(api.resizeJob).mockResolvedValue({ name: 'el', namespace: 'default', desiredNodes: 2, minNodes: 2, maxNodes: 4 })
    mount(job({ desired: 3 }))
    expect(screen.getByText(/Running 3 of 2–4 workers/)).toBeInTheDocument()
    const input = screen.getByLabelText('Desired workers') as HTMLInputElement
    const button = screen.getByRole('button', { name: 'Resize' })
    expect(button).toBeDisabled() // unchanged
    fireEvent.change(input, { target: { value: '5' } })
    expect(button).toBeDisabled() // above nodes
    fireEvent.change(input, { target: { value: '1' } })
    expect(button).toBeDisabled() // below minNodes
    fireEvent.change(input, { target: { value: '2' } })
    expect(button).toBeEnabled()
    fireEvent.click(button)
    await waitFor(() => expect(api.resizeJob).toHaveBeenCalledWith('el', 2))
  })

  it('explains why a finished or Kueue-managed job cannot be resized', () => {
    mount(job({ phase: 'Succeeded' }))
    expect(screen.getByText('The job is succeeded.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Resize' })).toBeNull()
    cleanup()
    mount(job({ labels: { 'kueue.x-k8s.io/queue-name': 'q' } }))
    expect(screen.getByText('Kueue picks the size of a Kueue-managed job.')).toBeInTheDocument()
  })
})
