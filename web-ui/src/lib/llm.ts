// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// LLM gateway: keys, published models and token usage (routes in services/api-gateway/routers/llm.py).

export interface LlmKey {
  id: string
  name: string
  namespace: string
  tenant: string
  prefix: string
  description?: string
  created?: string
}

export interface CreatedLlmKey extends LlmKey {
  key: string
  gatewayURL: string
}

export interface LlmModel {
  model: string
  namespace: string
  service: string
  servedModel: string
  shared: boolean
  ready: boolean
  phase: string
  priceInputPer1M: number | null
  priceOutputPer1M: number | null
}

export interface LlmModels {
  items: LlmModel[]
  gatewayURL: string
  enabled: boolean
}

export interface LlmUsageGroup {
  inputTokens: number
  outputTokens: number
  cost: number
  [key: string]: string | number
}

export interface LlmQuota {
  name: string
  namespaces: string[]
  tokensPerDay: number
  usedToday: number
}

export interface LlmUsage {
  groupBy: 'model' | 'tenant' | 'namespace' | 'day'
  items: LlmUsageGroup[]
  total: { inputTokens: number; outputTokens: number; cost: number }
  currency: string
  quotas: LlmQuota[]
}

const KEY_NAME = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/

/** Why a key name is not accepted, or undefined when it is. */
export function keyNameError(name: string): string | undefined {
  if (!name) return 'Name is required'
  if (!KEY_NAME.test(name)) return 'Lowercase letters, digits and "-" (at most 63)'
  return undefined
}

/** "0.50 / 1.00" per million tokens, "—" for a missing price (the gateway's default applies). */
export function priceLabel(m: Pick<LlmModel, 'priceInputPer1M' | 'priceOutputPer1M'>): string {
  const p = (v: number | null) => (v === null || v === undefined ? '—' : v.toFixed(2))
  return `${p(m.priceInputPer1M)} / ${p(m.priceOutputPer1M)}`
}

/** Share of the daily token quota used, 0–100 (capped). */
export function quotaPercent(q: LlmQuota): number {
  if (!q.tokensPerDay) return 0
  return Math.min(100, Math.round((q.usedToday / q.tokensPerDay) * 1000) / 10)
}

export function quotaTone(q: LlmQuota): 'ok' | 'warn' | 'bad' {
  const p = quotaPercent(q)
  return p >= 100 ? 'bad' : p >= 80 ? 'warn' : 'ok'
}

/** A curl line that calls the gateway with a key (placeholder when the key is not known). */
export function curlExample(gatewayURL: string, model: string, key = '$GRYVIA_LLM_KEY'): string {
  const base = gatewayURL || 'http://<llm-gateway>:8080'
  return `curl ${base}/v1/chat/completions -H "Authorization: Bearer ${key}" -H "Content-Type: application/json" -d '{"model":"${model}","messages":[{"role":"user","content":"Hello"}]}'`
}
