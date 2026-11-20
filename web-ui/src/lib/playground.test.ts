// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { DEFAULT_SETTINGS, buildBody, curlSnippet, keyError, parseSSE, pythonSnippet, readChunk, replyText, usageLabel } from './playground'

const settings = { ...DEFAULT_SETTINGS, model: 'chat', temperature: 0.2, maxTokens: 64 }

describe('playground helpers', () => {
  it('checks keys', () => {
    expect(keyError('')).toMatch(/Paste/)
    expect(keyError('sk-123')).toMatch(/gk-/)
    expect(keyError('gk-short')).toMatch(/gk-/)
    expect(keyError('gk-' + 'a'.repeat(43))).toBeUndefined()
  })

  it('builds the body with the system prompt first and stream only when on', () => {
    const history = [{ role: 'user' as const, content: 'Hi' }]
    expect(buildBody({ ...settings, systemPrompt: '  Be brief. ' }, history)).toEqual({
      model: 'chat',
      messages: [{ role: 'system', content: 'Be brief.' }, { role: 'user', content: 'Hi' }],
      temperature: 0.2,
      max_tokens: 64,
      stream: true,
    })
    const plain = buildBody({ ...settings, stream: false }, history)
    expect(plain.messages).toEqual(history)
    expect('stream' in plain).toBe(false)
  })

  it('parses SSE across chunk boundaries', () => {
    const first = parseSSE('data: {"a":1}\n\ndata: {"b"')
    expect(first).toEqual({ data: ['{"a":1}'], rest: 'data: {"b"' })
    const second = parseSSE(first.rest + ':2}\r\n\r\n: comment\n\ndata: [DONE]\n\n')
    expect(second).toEqual({ data: ['{"b":2}', '[DONE]'], rest: '' })
  })

  it('reads deltas, usage, errors and DONE', () => {
    expect(readChunk('{"choices":[{"delta":{"content":"Hi"}}]}')).toEqual({ delta: 'Hi' })
    expect(readChunk('{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}')).toEqual({
      usage: { prompt_tokens: 3, completion_tokens: 2 },
    })
    expect(readChunk('{"error":{"message":"stream interrupted"}}')).toEqual({ error: 'stream interrupted' })
    expect(readChunk('[DONE]')).toEqual({ done: true })
    expect(readChunk('not json')).toEqual({})
  })

  it('labels usage and reads plain replies', () => {
    expect(usageLabel({ prompt_tokens: 12, completion_tokens: 30 })).toBe('12 in · 30 out')
    expect(usageLabel()).toBe('no usage reported')
    expect(replyText({ choices: [{ message: { content: 'Hello' } }] })).toBe('Hello')
    expect(replyText({})).toBe('')
  })

  it('writes curl and Python snippets without the key', () => {
    const body = buildBody({ ...settings, systemPrompt: "It's fine" }, [{ role: 'user', content: 'Hi' }])
    const curl = curlSnippet('http://gw:8080', body)
    expect(curl).toContain('curl -N http://gw:8080/v1/chat/completions')
    expect(curl).toContain('Bearer $GRYVIA_LLM_KEY')
    expect(curl).toContain(`It'\\''s fine`)
    expect(curlSnippet('', { ...body, stream: undefined })).toContain('curl http://<llm-gateway>:8080/')
    const py = pythonSnippet('http://gw:8080', body)
    expect(py).toContain('base_url="http://gw:8080/v1"')
    expect(py).toContain('api_key=os.environ["GRYVIA_LLM_KEY"]')
    expect(py).toContain('model="chat"')
    expect(py).toContain('temperature=0.2')
    expect(py).toContain('max_tokens=64')
    expect(py).toContain('stream=True')
    expect(pythonSnippet('', { ...body, stream: undefined })).toContain('print(reply.choices[0].message.content)')
  })
})
