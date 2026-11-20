// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Pure helpers for the Experiments page. The gateway already ranks the leaderboard by the experiment's
// direction and adds deltaToBest/deltaPct; nothing here recomputes the ranking.
export interface LeaderboardEntry {
  rank?: number;
  job: string;
  primaryMetricValue?: number;
  status?: string;
  reason?: string;
  deltaToBest?: number;
  deltaPct?: number | null;
}

export interface Experiment {
  metadata?: { name?: string; namespace?: string; createdAt?: string };
  spec?: {
    description?: string;
    primaryMetric?: string;
    direction?: string;
    jobs?: { name?: string; jobRef?: string }[];
  };
  status?: {
    phase?: string;
    startedAt?: string;
    leaderboard?: LeaderboardEntry[];
    gpuHoursSaved?: number;
    costSaved?: number;
    anomaliesDetected?: number;
  };
}

export const experimentName = (e: Experiment) => e.metadata?.name ?? "unknown";

/** The best entry, i.e. the first of the gateway-ranked leaderboard that has a numeric metric. */
export function leaderOf(e: Experiment): LeaderboardEntry | undefined {
  return (e.status?.leaderboard ?? []).find(
    (r) => typeof r.primaryMetricValue === "number",
  );
}

/** "+25%" / "best" / "—" for the gap of an entry to the best one. */
export function gapLabel(r: LeaderboardEntry): string {
  if (typeof r.deltaToBest !== "number") return "—";
  if (r.deltaToBest === 0) return "best";
  if (r.deltaPct === null || r.deltaPct === undefined)
    return `${r.deltaToBest > 0 ? "+" : ""}${r.deltaToBest}`;
  const sign = r.deltaPct > 0 ? "+" : "";
  return `${sign}${r.deltaPct.toFixed(1)}%`;
}

export function directionLabel(direction?: string): string {
  if (direction === "minimize") return "lower is better";
  if (direction === "maximize") return "higher is better";
  return "direction not set";
}
