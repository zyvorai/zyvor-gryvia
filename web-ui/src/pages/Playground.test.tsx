// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { api } from '@/lib/api'
import type { LlmModels } from '@/lib/llm'
import Playground from './Playground'

vi.mock('@/lib/api', () => ({ api: { getLlmModels: vi.fn(), llmChat: vi.fn(), streamLlmChat: vi.fn() } }))
vi.mock('@/lib/notify', () => ({ notify: { error: vi.fn(), success: vi.fn() } }))
afterEach(() => {
  cleanup()
  vi.resetAllMocks()
})

const KEY = 'gk-' + 'a'.repeat(43)
const model = (name: string, ready = true) => ({
  model: name, namespace: 'ml', service: name, servedModel: name, shared: false, ready, phase: ready ? 'Ready' : 'Pending',
  priceInputPer1M: null, priceOutputPer1M: null,
})
const models = (items = [model('chat'), model('cold', false)]): LlmModels => ({ items, gatewayURL: 'http://gw:8080', enabled: true })

function mount() {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <Playground />
    </QueryClientProvider>,
  )
}

async function ready() {
  mount()
  await screen.findByLabelText('Model')
  fireEvent.change(screen.getByLabelText('LLM key'), { target: { value: KEY } })
  fireEvent.change(screen.getByLabelText('System prompt'), { target: { value: 'Be brief.' } })
  fireEvent.change(screen.getByRole('textbox', { name: 'Chat' }), { target: { value: 'Hello' } })
}

describe('Playground', () => {
  it('offers only ready models and needs a key before sending', async () => {
    vi.mocked(api.getLlmModels).mockResolvedValue(models())
    mount()
    const select = await screen.findByLabelText('Model')
    expect(Array.from((select as HTMLSelectElement).options).map((o) => o.value)).toEqual(['chat'])
    fireEvent.change(screen.getByRole('textbox', { name: 'Chat' }), { target: { value: 'Hello' } })
    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled()
    fireEvent.change(screen.getByLabelText('LLM key'), { target: { value: 'sk-x' } })
    expect(screen.getByText('An LLM key starts with gk-')).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText('LLM key'), { target: { value: KEY } })
    expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled()
  })

  it('streams a reply with its usage, sending the key and the settings', async () => {
    vi.mocked(api.getLlmModels).mockResolvedValue(models())
    vi.mocked(api.streamLlmChat).mockImplementation(async (_body, _key, onChunk) => {
      onChunk({ delta: 'Hi ' })
      onChunk({ delta: 'there.' })
      onChunk({ usage: { prompt_tokens: 9, completion_tokens: 2 } })
    })
    await ready()
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    expect(await screen.findByText('Hi there.')).toBeInTheDocument()
    expect(await screen.findByTestId('usage')).toHaveTextContent('9 in · 2 out')
    const [body, key] = vi.mocked(api.streamLlmChat).mock.calls[0]
    expect(key).toBe(KEY)
    expect(body).toEqual({
      model: 'chat',
      messages: [{ role: 'system', content: 'Be brief.' }, { role: 'user', content: 'Hello' }],
      temperature: 0.7,
      max_tokens: 256,
      stream: true,
    })
  })

  it('sends the whole conversation on the next turn, without streaming when it is off', async () => {
    vi.mocked(api.getLlmModels).mockResolvedValue(models())
    vi.mocked(api.llmChat).mockResolvedValue({ text: 'Hello!', usage: { prompt_tokens: 5, completion_tokens: 1 } })
    await ready()
    fireEvent.click(screen.getByLabelText('Stream the reply'))
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    expect(await screen.findByText('Hello!')).toBeInTheDocument()
    fireEvent.change(screen.getByRole('textbox', { name: 'Chat' }), { target: { value: 'Again' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(api.llmChat).toHaveBeenCalledTimes(2))
    expect(vi.mocked(api.llmChat).mock.calls[1][0].messages).toEqual([
      { role: 'system', content: 'Be brief.' },
      { role: 'user', content: 'Hello' },
      { role: 'assistant', content: 'Hello!' },
      { role: 'user', content: 'Again' },
    ])
    expect(api.streamLlmChat).not.toHaveBeenCalled()
  })

  it('shows an error on the turn and leaves it out of the next request', async () => {
    vi.mocked(api.getLlmModels).mockResolvedValue(models())
    vi.mocked(api.streamLlmChat).mockRejectedValueOnce(new Error('429: token quota exceeded'))
    await ready()
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('429: token quota exceeded')
    vi.mocked(api.streamLlmChat).mockResolvedValueOnce(undefined)
    fireEvent.change(screen.getByRole('textbox', { name: 'Chat' }), { target: { value: 'Retry' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(api.streamLlmChat).toHaveBeenCalledTimes(2))
    expect(vi.mocked(api.streamLlmChat).mock.calls[1][0].messages).toEqual([
      { role: 'system', content: 'Be brief.' },
      { role: 'user', content: 'Hello' },
      { role: 'user', content: 'Retry' },
    ])
  })

  it('shows a curl or Python snippet of the current settings without the key', async () => {
    vi.mocked(api.getLlmModels).mockResolvedValue(models())
    await ready()
    const snippet = screen.getByTestId('snippet')
    expect(snippet).toHaveTextContent('http://gw:8080/v1/chat/completions')
    expect(snippet).toHaveTextContent('"content":"Hello"')
    expect(snippet.textContent).not.toContain(KEY)
    fireEvent.click(screen.getByRole('tab', { name: 'Python' }))
    expect(screen.getByTestId('snippet')).toHaveTextContent('from openai import OpenAI')
  })

  it('says when no model is ready', async () => {
    vi.mocked(api.getLlmModels).mockResolvedValue(models([model('cold', false)]))
    mount()
    expect(await screen.findByText('No ready models.')).toBeInTheDocument()
  })
})
