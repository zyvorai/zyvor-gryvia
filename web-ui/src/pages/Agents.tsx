// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import {
  canChat,
  displayPhase,
  replicasLabel,
  replySummary,
  replyText,
  toolLabel,
  type Agent,
  type ChatMessage,
} from "@/lib/agents";
import { notify } from "@/lib/notify";
import { phaseTone } from "@/lib/phase";
import { product, proposalIdIn, zyntraDecisionLink } from "@/lib/sovereign";
import type { SortAccessor } from "@/lib/tableState";
import type { Column } from "@/components/DataTable";
import ResourceListPage from "@/components/ResourceListPage";

const QUERY_KEY = "agents";

const SORTS: Record<string, SortAccessor<Agent>> = {
  name: (a) => a.metadata.name,
  phase: (a) => displayPhase(a),
};

const columns: Column<Agent>[] = [
  {
    key: "name",
    header: "Agent",
    sortable: true,
    render: (a) => <span className="mono">{a.metadata.name}</span>,
  },
  { key: "model", header: "Model", render: (a) => <span className="mono">{a.spec.model ?? "—"}</span> },
  {
    key: "phase",
    header: "Phase",
    sortable: true,
    render: (a) => (
      <span className={`pill ${phaseTone(displayPhase(a))}`} title={a.status.message}>
        {displayPhase(a)}
      </span>
    ),
  },
  { key: "replicas", header: "Ready", render: (a) => replicasLabel(a) },
  { key: "tools", header: "Tools", render: (a) => String(a.spec.tools?.length ?? 0) },
];

interface Turn {
  message: ChatMessage;
  summary?: string;
}

function ChatPanel({ agent }: { agent: Agent }) {
  const name = agent.metadata.name;
  const [turns, setTurns] = useState<Turn[]>([]);
  const [draft, setDraft] = useState("");
  const send = useMutation({
    mutationFn: (messages: ChatMessage[]) => api.chatAgent(name, messages),
    onSuccess: (reply) =>
      setTurns((t) => [...t, { message: { role: "assistant", content: replyText(reply) }, summary: replySummary(reply) }]),
    onError: (err) => notify.error(`${name} did not answer`, err),
  });
  const submit = () => {
    const content = draft.trim();
    if (!content || send.isPending) return;
    const next = [...turns, { message: { role: "user" as const, content } }];
    setTurns(next);
    setDraft("");
    send.mutate(next.map((t) => t.message));
  };
  const ready = canChat(agent);
  const usesZyntra = (agent.spec.tools ?? []).some((t) => t.type === "zyntra");
  const sovereign = useQuery({ queryKey: ["sovereign"], queryFn: api.getSovereign, enabled: usesZyntra, staleTime: 60_000 });
  const zyntraConsole = product(sovereign.data, "Zyntra")?.console;
  const proposalLink = (text: string) => {
    const id = proposalIdIn(text);
    const href = id && zyntraDecisionLink(zyntraConsole, id);
    return href ? (
      <a href={href} target="_blank" rel="noopener noreferrer">
        Review {id} in Zyntra
      </a>
    ) : null;
  };
  return (
    <div style={{ marginTop: "1rem" }}>
      <p className="eyebrow" id={`chat-${name}`}>
        Chat
      </p>
      {turns.length > 0 && (
        <ol aria-live="polite" style={{ listStyle: "none", padding: 0, display: "grid", gap: "0.75rem" }}>
          {turns.map((t, i) => (
            <li key={i}>
              <strong>{t.message.role === "user" ? "You" : name}</strong>
              <p style={{ whiteSpace: "pre-wrap", margin: "0.25rem 0" }}>{t.message.content}</p>
              {t.message.role === "assistant" && proposalLink(t.message.content)}
              {t.summary && <small className="muted">{t.summary}</small>}
            </li>
          ))}
        </ol>
      )}
      <textarea
        aria-labelledby={`chat-${name}`}
        rows={3}
        className="codeedit compact"
        placeholder={ready ? "Ask the agent…" : "The agent is not ready"}
        disabled={!ready}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) submit();
        }}
      />
      <div style={{ display: "flex", gap: "0.5rem", marginTop: "0.5rem" }}>
        <button type="button" className="primary" disabled={!ready || !draft.trim() || send.isPending} onClick={submit}>
          {send.isPending ? "Thinking…" : "Send"}
        </button>
        <button type="button" className="btn-secondary" disabled={turns.length === 0} onClick={() => setTurns([])}>
          Clear
        </button>
      </div>
    </div>
  );
}

function AgentCard({ agent }: { agent: Agent }) {
  const queryClient = useQueryClient();
  const name = agent.metadata.name;
  const scale = useMutation({
    mutationFn: (replicas: number) => api.scaleAgent(name, replicas),
    onSuccess: (_d, replicas) => {
      notify.success(replicas === 0 ? `Scaled ${name} to zero` : `Scaled ${name} to ${replicas}`);
      return queryClient.invalidateQueries({ queryKey: [QUERY_KEY] });
    },
    onError: (err) => notify.error(`Could not scale ${name}`, err),
  });
  const st = agent.status;
  const rows: [string, string][] = [
    ["Model", agent.spec.model ?? "—"],
    ["Endpoint", st.endpoint ?? "—"],
    ["Replicas ready", replicasLabel(agent)],
    ["Max steps", String(agent.spec.maxSteps ?? 5)],
    ["Tools", (agent.spec.tools ?? []).map(toolLabel).join(", ") || "none"],
    ["Gateway key Secret", st.keySecret ?? "—"],
  ];
  const scaledDown = agent.spec.replicas === 0;
  return (
    <section className="card span3">
      <p className="eyebrow">Agent</p>
      <h2 className="card-title">
        <span className="mono">{name}</span>
      </h2>
      {st.message && <p>{st.message}</p>}
      {agent.spec.systemPrompt && (
        <>
          <p className="eyebrow">System prompt</p>
          <pre className="mono" style={{ whiteSpace: "pre-wrap" }}>
            {agent.spec.systemPrompt}
          </pre>
        </>
      )}
      <div className="table-wrap">
        <table>
          <caption className="sr-only">Details of {name}</caption>
          <tbody>
            {rows.map(([k, v]) => (
              <tr key={k}>
                <th scope="row">{k}</th>
                <td className="mono">{v}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div style={{ display: "flex", gap: "0.5rem", marginTop: "1rem" }}>
        <button
          type="button"
          className="btn-secondary"
          disabled={scale.isPending}
          onClick={() => scale.mutate(scaledDown ? 1 : 0)}
        >
          {scaledDown ? "Scale up" : "Scale to zero"}
        </button>
      </div>
      <ChatPanel key={name} agent={agent} />
    </section>
  );
}

export default function Agents() {
  return (
    <ResourceListPage<Agent>
      title="Agents"
      noun="agent"
      eyebrow="Agents"
      heroTitle="Models that use your tools."
      lede="Each agent runs a tool-calling loop against a model on the LLM gateway, with its own key: it can search your vector indexes, call allowlisted HTTP endpoints, and read Zyntra's ontology and propose actions for people to approve. Applications call its OpenAI-compatible endpoint; you can try it here."
      queryKey={QUERY_KEY}
      fetch={api.getAgents}
      remove={api.deleteAgent}
      columns={columns}
      sorts={SORTS}
      searchText={(a) => `${a.metadata.name} ${a.spec.model ?? ""} ${displayPhase(a)}`}
      createHint="gryvia agents create -f examples/agents/agent.yaml"
      deleteNote="Its runtime Deployment, Service, NetworkPolicy and gateway key are deleted."
      detail={(a) => <AgentCard agent={a} />}
    />
  );
}
