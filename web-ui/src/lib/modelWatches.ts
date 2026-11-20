// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Pure helpers for the Model factory page (GryviaModelWatch, see docs/model-factory.md).
import { phaseTone, type Tone } from "./phase";

export interface ModelWatchSource {
  provider?: string;
  author?: string;
  nameRegex?: string;
  pipelineTag?: string;
}

export interface ModelWatch {
  metadata: { name: string; namespace?: string; creationTimestamp?: string };
  spec: {
    sources?: ModelWatchSource[];
    licenseAllowlist?: string[];
    maxParamsB?: number;
    pollInterval?: string;
    maxConcurrentRuns?: number;
    suspend?: boolean;
    steps?: string[];
  };
  status: {
    phase?: string;
    message?: string;
    lastPollTime?: string;
    nextPollTime?: string;
    activeRuns?: number;
    /** Candidate count per phase. */
    candidates?: Record<string, number>;
  };
}

export interface ModelWatchRun {
  model: string;
  revision?: string;
  paramsB?: string;
  license?: string;
  gpus?: number;
  phase?: string;
  workflow?: string;
  firstSeen?: string;
  message?: string;
}

/** "Qwen, meta-llama" — the authors watched, or the provider when a source has no author. */
export function sourcesLabel(w: Pick<ModelWatch, "spec">): string {
  const names = (w.spec.sources ?? []).map(
    (s) => s.author || s.provider || "huggingface",
  );
  return names.length ? names.join(", ") : "—";
}

export function watchTone(phase?: string): Tone {
  return phase === "Watching" ? "ok" : phaseTone(phase);
}

export function candidateCount(
  w: Pick<ModelWatch, "status">,
  phase: string,
): number {
  return w.status.candidates?.[phase] ?? 0;
}

/** Runs worth showing by default: baseline models (seen on the first poll, never run) are hidden. */
export function visibleRuns(
  runs: ModelWatchRun[],
  showBaseline: boolean,
): ModelWatchRun[] {
  return showBaseline ? runs : runs.filter((r) => r.phase !== "Baseline");
}

export function shortRevision(rev?: string): string {
  return rev ? rev.slice(0, 7) : "—";
}
