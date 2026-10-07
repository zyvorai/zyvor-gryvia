import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { intelligenceApi } from '@/lib/intelligence'
import Intelligence from './Intelligence'

vi.mock('@/lib/useRole', () => ({ useIsAdmin: () => false }))
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

function mount() {
  vi.spyOn(intelligenceApi, 'schemas').mockResolvedValue({
    economics: {
      type: 'object',
      required: ['cost', 'currency'],
      properties: {
        cost: { type: 'number', minimum: 0 },
        currency: { type: 'string' },
        deliveredTokens: { type: 'integer', minimum: 0 },
      },
    },
  })
  vi.spyOn(intelligenceApi, 'capabilities').mockResolvedValue({
    actionsEnabled: false,
    limitations: [],
  })
  const actions = vi.spyOn(intelligenceApi, 'actions')
  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <Intelligence />
    </QueryClientProvider>,
  )
  return actions
}

describe('intelligence workbench', () => {
  it('submits supplied fields and keeps optional unknown metrics absent', async () => {
    const analyze = vi
      .spyOn(intelligenceApi, 'analyze')
      .mockResolvedValue({
        mode: 'estimate',
        costPerMillionDeliveredTokens: null,
      })
    const actions = mount()
    await userEvent.selectOptions(
      screen.getByLabelText('Analysis'),
      'economics',
    )
    await userEvent.type(await screen.findByLabelText('Currency'), 'USD')
    await userEvent.clear(screen.getByLabelText('Cost'))
    await userEvent.type(screen.getByLabelText('Cost'), '10')
    await userEvent.click(screen.getByRole('button', { name: 'Analyze' }))
    expect(await screen.findByText('Unknown')).toBeInTheDocument()
    expect(analyze).toHaveBeenCalledWith('economics', {
      cost: 10,
      currency: 'USD',
    })
    expect(actions).not.toHaveBeenCalled()
    expect(screen.queryByText('Approved operations')).not.toBeInTheDocument()
  })

  it('shows live read failures and removes a previous report', async () => {
    vi.spyOn(intelligenceApi, 'explain').mockRejectedValue(
      new Error('Job not found in your namespaces'),
    )
    mount()
    await userEvent.type(screen.getByLabelText('Job name'), 'train')
    await userEvent.click(
      screen.getByRole('button', { name: 'Explain scheduling' }),
    )
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Job not found in your namespaces',
    )
    expect(screen.queryByText('Download full report')).not.toBeInTheDocument()
  })
})
