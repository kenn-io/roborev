import { describe, expect, it } from "vitest";

import type {
  AnalyticsSnapshot,
  AnalyticsSummary,
} from "../api/generated/models";
import { OTHER_KEY, reviewLatency, seriesGroups } from "./analytics-series";

function summary(
  reviews: number,
  failed: number,
  cost: number,
): AnalyticsSummary {
  const latency = { p50_secs: 60, p90_secs: 120, p99_secs: 180 };
  return {
    reviews: {
      total: reviews,
      done: reviews,
      failed: 0,
      canceled: 0,
      skipped: 0,
      failure_rate: 0,
      run_errors: 0,
      run_error_rate: 0,
    },
    verdicts: {
      passed: reviews - failed,
      fail_open: failed,
      fail_closed: 0,
      rated: reviews,
      failure_rate: reviews > 0 ? failed / reviews : 0,
    },
    review_latency: latency,
    attempts: { eligible: reviews, duration: latency },
    cost: {
      total_usd: cost,
      eligible_attempts: reviews,
      priced_attempts: reviews,
      coverage: 1,
      complete: true,
    },
  };
}

function snapshot(values: string[]): AnalyticsSnapshot {
  const bucket = {
    start: "2026-08-01T00:00:00Z",
    end: "2026-08-02T00:00:00Z",
  };
  return {
    schema_version: 1,
    filters: {
      until: "2026-08-02T00:00:00Z",
      projects: [],
      sources: [],
      agents: [],
      models: [],
      bucket: "day",
      split: values.length > 0 ? "model" : undefined,
    },
    summary: summary(100, 10, 50),
    time_series: [{ ...bucket, ...summary(100, 10, 50) }],
    projects: [],
    sources: [],
    agents: [],
    models: [],
    split_series: values.map((value, index) => ({
      value,
      summary: summary(20 - index, 1, 2),
      time_series: [{ ...bucket, ...summary(20 - index, 1, 2) }],
    })),
    options: { projects: [], sources: [], agents: [], models: [] },
  };
}

describe("seriesGroups", () => {
  it("returns one group for the whole selection without a split", () => {
    const groups = seriesGroups(snapshot([]), (value) => value);

    expect(groups.map((group) => group.label)).toEqual(["All reviews"]);
    expect(groups[0]?.buckets[0]?.reviews.total).toBe(100);
  });

  it("gives up to eight values their own palette slot in rank order", () => {
    const values = ["a", "b", "c", "d", "e", "f", "g", "h"];
    const groups = seriesGroups(snapshot(values), (value) => `model ${value}`);

    expect(groups.map((group) => group.key)).toEqual(values);
    expect(groups[0]?.label).toBe("model a");
    expect(groups[0]?.color).toBe("var(--series-1)");
    expect(groups[7]?.color).toBe("var(--series-8)");
  });

  it("folds values past the seventh into one Other group", () => {
    const values = ["a", "b", "c", "d", "e", "f", "g", "h", "i"];
    const groups = seriesGroups(snapshot(values), (value) => value);

    expect(groups).toHaveLength(8);
    const other = groups[7];
    expect(other?.key).toBe(OTHER_KEY);
    expect(other?.label).toBe("Other (2)");
    expect(other?.color).toBe("var(--series-other)");
    expect(other?.members).toBe(2);
    // h has 13 reviews and i has 12; each has one failed verdict and $2.
    const bucket = other?.buckets[0];
    expect(bucket?.reviews.total).toBe(25);
    expect(bucket?.verdicts.failure_rate).toBeCloseTo(2 / 25);
    expect(bucket?.cost.total_usd).toBe(4);
    expect(other?.summary.reviews.total).toBe(25);
    // Percentiles cannot be combined, so the folded latency is unknown.
    expect(bucket && reviewLatency(bucket, "p50_secs")).toBeNull();
  });
});
