// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from "vitest";
import {
  candidateCount,
  shortRevision,
  sourcesLabel,
  visibleRuns,
  watchTone,
  type ModelWatch,
} from "./modelWatches";

const watch: ModelWatch = {
  metadata: { name: "open-llms" },
  spec: {
    sources: [
      { provider: "huggingface", author: "Qwen" },
      { provider: "huggingface" },
    ],
  },
  status: { phase: "Watching", candidates: { Running: 1, Baseline: 4 } },
};

describe("model watches", () => {
  it("labels sources by author, falling back to the provider", () => {
    expect(sourcesLabel(watch)).toBe("Qwen, huggingface");
    expect(sourcesLabel({ spec: {} })).toBe("—");
  });

  it("maps phases to tones", () => {
    expect(watchTone("Watching")).toBe("ok");
    expect(watchTone("Failed")).toBe("bad");
    expect(watchTone("Suspended")).toBe("");
  });

  it("counts candidates per phase", () => {
    expect(candidateCount(watch, "Running")).toBe(1);
    expect(candidateCount(watch, "Failed")).toBe(0);
  });

  it("hides baseline runs unless asked", () => {
    const runs = [
      { model: "a", phase: "Baseline" },
      { model: "b", phase: "Running" },
    ];
    expect(visibleRuns(runs, false).map((r) => r.model)).toEqual(["b"]);
    expect(visibleRuns(runs, true)).toHaveLength(2);
  });

  it("shortens revisions", () => {
    expect(shortRevision("0123456789abcdef")).toBe("0123456");
    expect(shortRevision()).toBe("—");
  });
});
