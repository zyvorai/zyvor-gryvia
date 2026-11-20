// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

export interface AgentTool {
  name: string;
  description?: string;
  type: "retrieval" | "http" | "zyntra";
  vectorIndexRef?: string;
  topK?: number;
  urls?: string[];
  method?: "GET" | "POST";
  url?: string;
  tokenSecret?: string;
  tokenKey?: string;
  propose?: boolean;
  actions?: string[];
}

export interface Agent {
  metadata: { name: string; namespace?: string; creationTimestamp?: string };
  spec: {
    model?: string;
    systemPrompt?: string;
    tools?: AgentTool[];
    maxSteps?: number;
    replicas?: number;
    image?: string;
  };
  status: {
    phase?: string;
    message?: string;
    endpoint?: string;
    readyReplicas?: number;
    keySecret?: string;
  };
}

export interface ChatMessage {
  role: "system" | "user" | "assistant";
  content: string;
}

export interface AgentToolCall {
  step: number;
  tool: string;
  arguments?: unknown;
  ok: boolean;
}

export interface AgentReply {
  choices: { message: { role: string; content: string }; finish_reason?: string }[];
  usage?: { prompt_tokens?: number; completion_tokens?: number; total_tokens?: number };
  gryvia?: { steps?: number; toolCalls?: AgentToolCall[] };
}

/** "search (retrieval: handbook)", "status (http GET: 2 URLs)" or "ops (Zyntra, read and propose)". */
export function toolLabel(t: AgentTool): string {
  if (t.type === "retrieval") return `${t.name} (retrieval: ${t.vectorIndexRef ?? "?"})`;
  if (t.type === "zyntra") return `${t.name} (Zyntra, ${t.propose ? "read and propose" : "read only"})`;
  const n = t.urls?.length ?? 0;
  return `${t.name} (http ${t.method ?? "GET"}: ${n} URL${n === 1 ? "" : "s"})`;
}

/** "Scaled to zero" when spec.replicas is 0, else the phase. */
export function displayPhase(a: Pick<Agent, "spec" | "status">): string {
  if (a.spec.replicas === 0) return "Scaled to zero";
  return a.status.phase ?? "Pending";
}

/** "1/2", from ready and desired replicas (desired defaults to 1). */
export function replicasLabel(a: Pick<Agent, "spec" | "status">): string {
  return `${a.status.readyReplicas ?? 0}/${a.spec.replicas ?? 1}`;
}

export function canChat(a: Pick<Agent, "status">): boolean {
  return a.status.phase === "Ready";
}

/** The answer text of a reply. */
export function replyText(r: AgentReply): string {
  return r.choices?.[0]?.message?.content ?? "";
}

/** "2 model calls · search ✓ · status ✗ · 42 tokens". */
export function replySummary(r: AgentReply): string {
  const steps = r.gryvia?.steps ?? 1;
  const parts = [`${steps} model call${steps === 1 ? "" : "s"}`];
  for (const c of r.gryvia?.toolCalls ?? []) parts.push(`${c.tool} ${c.ok ? "✓" : "✗"}`);
  if (r.usage?.total_tokens) parts.push(`${r.usage.total_tokens} tokens`);
  if (r.choices?.[0]?.finish_reason === "length") parts.push("stopped at maxSteps");
  return parts.join(" · ");
}
