// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Model playground: chat completions through POST /api/llm/chat with the caller's LLM key (services/api-gateway/routers/llm.py).

export interface PlaygroundMessage {
  role: 'system' | 'user' | 'assistant'
  content: string
}

export interface PlaygroundSettings {
  model: string
  systemPrompt: string
  temperature: number
  maxTokens: number
  stream: boolean
}

export interface TokenUsage {
  prompt_tokens?: number
  completion_tokens?: number
  total_tokens?: number
}

export interface ChatRequestBody {
  model: string
  messages: PlaygroundMessage[]
  temperature: number
  max_tokens: number
  stream?: boolean
}

export const DEFAULT_SETTINGS: PlaygroundSettings = { model: '', systemPrompt: '', temperature: 0.7, maxTokens: 256, stream: true }

const KEY = /^gk-[A-Za-z0-9_-]{20,128}$/

/** Why a key is not accepted, or undefined when it looks like a gateway key. */
export function keyError(key: string): string | undefined {
  if (!key) return 'Paste an LLM key (create one on the LLM gateway page)'
  if (!KEY.test(key)) return 'An LLM key starts with gk-'
  return undefined
}

/** The request body for the conversation so far; the system prompt goes first when set. */
export function buildBody(s: PlaygroundSettings, history: PlaygroundMessage[]): ChatRequestBody {
  const system = s.systemPrompt.trim()
  const messages = system ? [{ role: 'system' as const, content: system }, ...history] : [...history]
  const body: ChatRequestBody = { model: s.model, messages, temperature: s.temperature, max_tokens: s.maxTokens }
  if (s.stream) body.stream = true
  return body
}

/** Splits buffered SSE text into complete data payloads; returns the unfinished tail to keep buffering. */
export function parseSSE(buffer: string): { data: string[]; rest: string } {
  const parts = buffer.replace(/\r\n/g, '\n').split('\n\n')
  const rest = parts.pop() ?? ''
  const data: string[] = []
  for (const event of parts) {
    const lines = event.split('\n').filter((l) => l.startsWith('data:'))
    if (lines.length) data.push(lines.map((l) => l.slice(5).replace(/^ /, '')).join('\n'))
  }
  return { data, rest }
}

export interface StreamChunk {
  delta?: string
  usage?: TokenUsage
  error?: string
  done?: boolean
}

/** One SSE data payload of an OpenAI chat stream: content delta, usage, an error event or [DONE]. */
export function readChunk(data: string): StreamChunk {
  if (data.trim() === '[DONE]') return { done: true }
  let obj: { choices?: { delta?: { content?: string } }[]; usage?: TokenUsage; error?: { message?: string } | string }
  try {
    obj = JSON.parse(data)
  } catch {
    return {}
  }
  const out: StreamChunk = {}
  const delta = obj.choices?.map((c) => c.delta?.content ?? '').join('')
  if (delta) out.delta = delta
  if (obj.usage) out.usage = obj.usage
  if (obj.error) out.error = typeof obj.error === 'string' ? obj.error : obj.error.message || 'stream error'
  return out
}

/** The reply text of a non-streamed chat completion. */
export function replyText(reply: { choices?: { message?: { content?: string } }[] }): string {
  return reply.choices?.map((c) => c.message?.content ?? '').join('') ?? ''
}

/** "12 in · 30 out" for a reply's token usage. */
export function usageLabel(u?: TokenUsage): string {
  if (!u || (u.prompt_tokens === undefined && u.completion_tokens === undefined)) return 'no usage reported'
  return `${u.prompt_tokens ?? 0} in · ${u.completion_tokens ?? 0} out`
}

const gatewayBase = (url: string) => url || 'http://<llm-gateway>:8080'

/** The current settings as a curl call to the gateway, with the key left as an environment variable. */
export function curlSnippet(gatewayURL: string, body: ChatRequestBody): string {
  const json = JSON.stringify(body).replace(/'/g, "'\\''")
  return [
    `curl ${body.stream ? '-N ' : ''}${gatewayBase(gatewayURL)}/v1/chat/completions \\`,
    '  -H "Authorization: Bearer $GRYVIA_LLM_KEY" -H "Content-Type: application/json" \\',
    `  -d '${json}'`,
  ].join('\n')
}

/** The current settings with the OpenAI Python client pointed at the gateway. */
export function pythonSnippet(gatewayURL: string, body: ChatRequestBody): string {
  const messages = JSON.stringify(body.messages, null, 4).replace(/\n/g, '\n    ')
  const lines = [
    'import os',
    'from openai import OpenAI',
    '',
    `client = OpenAI(base_url="${gatewayBase(gatewayURL)}/v1", api_key=os.environ["GRYVIA_LLM_KEY"])`,
    'reply = client.chat.completions.create(',
    `    model=${JSON.stringify(body.model)},`,
    `    messages=${messages},`,
    `    temperature=${body.temperature},`,
    `    max_tokens=${body.max_tokens},`,
  ]
  if (body.stream) {
    lines.push('    stream=True,', ')', 'for chunk in reply:', '    if chunk.choices:', '        print(chunk.choices[0].delta.content or "", end="")')
  } else {
    lines.push(')', 'print(reply.choices[0].message.content)')
  }
  return lines.join('\n')
}
