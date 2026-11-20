// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from "vitest";
import { canChat, displayPhase, replicasLabel, replySummary, replyText, toolLabel, type Agent } from "./agents";

const agent = (spec: Agent["spec"] = {}, status: Agent["status"] = {}): Agent => ({
  metadata: { name: "helper" },
  spec,
  status,
});

describe("agent helpers", () => {
  it("labels tools", () => {
    expect(toolLabel({ name: "search", type: "retrieval", vectorIndexRef: "handbook" })).toBe(
      "search (retrieval: handbook)",
    );
    expect(toolLabel({ name: "status", type: "http", urls: ["http://a/", "http://b/"] })).toBe(
      "status (http GET: 2 URLs)",
    );
    expect(toolLabel({ name: "hook", type: "http", method: "POST", urls: ["http://a/"] })).toBe(
      "hook (http POST: 1 URL)",
    );
    expect(toolLabel({ name: "ops", type: "zyntra", url: "http://z/", propose: true })).toBe(
      "ops (Zyntra, read and propose)",
    );
    expect(toolLabel({ name: "ops", type: "zyntra", url: "http://z/" })).toBe("ops (Zyntra, read only)");
  });

  it("shows scale to zero and replicas", () => {
    expect(displayPhase(agent())).toBe("Pending");
    expect(displayPhase(agent({}, { phase: "Ready" }))).toBe("Ready");
    expect(displayPhase(agent({ replicas: 0 }, { phase: "Pending" }))).toBe("Scaled to zero");
    expect(replicasLabel(agent())).toBe("0/1");
    expect(replicasLabel(agent({ replicas: 2 }, { readyReplicas: 1 }))).toBe("1/2");
  });

  it("allows chat only when ready", () => {
    expect(canChat(agent({}, { phase: "Ready" }))).toBe(true);
    expect(canChat(agent({}, { phase: "Pending" }))).toBe(false);
  });

  it("summarizes replies", () => {
    const reply = {
      choices: [{ message: { role: "assistant", content: "Monthly." }, finish_reason: "stop" }],
      usage: { total_tokens: 42 },
      gryvia: {
        steps: 2,
        toolCalls: [
          { step: 1, tool: "search", ok: true },
          { step: 1, tool: "status", ok: false },
        ],
      },
    };
    expect(replyText(reply)).toBe("Monthly.");
    expect(replySummary(reply)).toBe("2 model calls · search ✓ · status ✗ · 42 tokens");
    expect(replySummary({ choices: [{ message: { role: "assistant", content: "" }, finish_reason: "length" }] })).toBe(
      "1 model call · stopped at maxSteps",
    );
  });
});
