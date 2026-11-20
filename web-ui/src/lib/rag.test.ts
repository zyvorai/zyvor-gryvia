// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from "vitest";
import { canReingest, contentLabel, displayPhase, retrieveCurl, storeLabel, type VectorIndex } from "./rag";

const idx = (spec: VectorIndex["spec"] = {}, status: VectorIndex["status"] = {}): VectorIndex => ({
  metadata: { name: "kb" },
  spec,
  status,
});

describe("rag helpers", () => {
  it("labels the store", () => {
    expect(storeLabel(idx())).toBe("managed Qdrant");
    expect(storeLabel(idx({ store: "external", storeURL: "https://q" }))).toBe("external https://q");
    expect(storeLabel(idx({ store: "external" }))).toBe("external");
  });

  it("labels the content", () => {
    expect(contentLabel(idx())).toBe("—");
    expect(contentLabel(idx({}, { chunks: 12, documents: 3 }))).toBe("12 chunks · 3 docs");
  });

  it("shows suspension over the phase", () => {
    expect(displayPhase(idx())).toBe("Pending");
    expect(displayPhase(idx({}, { phase: "Ready" }))).toBe("Ready");
    expect(displayPhase(idx({ suspend: true }, { phase: "Ready" }))).toBe("Suspended");
  });

  it("allows reingest unless suspended or ingesting", () => {
    expect(canReingest(idx({}, { phase: "Ready" }))).toBe(true);
    expect(canReingest(idx({}, { phase: "Failed" }))).toBe(true);
    expect(canReingest(idx({}, { phase: "Ingesting" }))).toBe(false);
    expect(canReingest(idx({ suspend: true }, { phase: "Ready" }))).toBe(false);
  });

  it("builds a curl example", () => {
    const c = retrieveCurl("http://gw:8080/v1/retrieve", "kb");
    expect(c).toContain("http://gw:8080/v1/retrieve");
    expect(c).toContain('"index":"kb"');
    expect(c).toContain("$GRYVIA_LLM_KEY");
    expect(retrieveCurl("", "kb")).toContain("<llm-gateway>");
  });
});
