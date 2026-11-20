// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from "vitest";
import { directionLabel, gapLabel, leaderOf } from "./experiments";

describe("experiments helpers", () => {
  it("takes the first numeric entry as the leader", () => {
    expect(
      leaderOf({
        status: {
          leaderboard: [
            { job: "x" },
            { job: "a", primaryMetricValue: 0.4 },
            { job: "b", primaryMetricValue: 0.5 },
          ],
        },
      })?.job,
    ).toBe("a");
    expect(leaderOf({})).toBeUndefined();
  });
  it("labels the gap to the best run", () => {
    expect(gapLabel({ job: "a", deltaToBest: 0, deltaPct: 0 })).toBe("best");
    expect(gapLabel({ job: "b", deltaToBest: 0.1, deltaPct: 25 })).toBe(
      "+25.0%",
    );
    expect(gapLabel({ job: "c", deltaToBest: -0.2, deltaPct: -10 })).toBe(
      "-10.0%",
    );
    expect(gapLabel({ job: "d", deltaToBest: 2, deltaPct: null })).toBe("+2");
    expect(gapLabel({ job: "e" })).toBe("—");
  });
  it("explains the direction", () => {
    expect(directionLabel("minimize")).toBe("lower is better");
    expect(directionLabel("maximize")).toBe("higher is better");
    expect(directionLabel(undefined)).toBe("direction not set");
  });
});
