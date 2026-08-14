import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { components } from "../api/generated";
import type {
  AnalyticsFilters,
  AnalyticsStore,
} from "../stores/analytics.svelte";
import AnalyticsView from "./AnalyticsView.svelte";

type AnalyticsSnapshot = components["schemas"]["AnalyticsSnapshot"];

const filters: AnalyticsFilters = {
  range: "30d",
  projects: [],
  sources: [],
  agent: "",
  model: "",
  bucket: "auto",
};

function makeSnapshot(
  total = 12,
  bucket: AnalyticsSnapshot["filters"]["bucket"] = "day",
): AnalyticsSnapshot {
  const percentiles = { p50_secs: 90, p90_secs: 180, p99_secs: 300 };
  const reviews = {
    total,
    done: 9,
    failed: 1,
    canceled: 1,
    skipped: 1,
    failure_rate: 0.1,
    run_errors: 1,
    run_error_rate: 0.1,
  };
  const attempts = { eligible: 4, duration: percentiles };
  const cost = {
    total_usd: 3.25,
    eligible_attempts: 4,
    priced_attempts: 2,
    coverage: 0.5,
    complete: false,
  };
  const summary = {
    reviews,
    verdicts: {
      passed: 7,
      fail_open: 1,
      fail_closed: 1,
      rated: 9,
      failure_rate: 2 / 9,
    },
    review_latency: percentiles,
    attempts,
    cost,
  };
  return {
    schema_version: 1,
    filters: {
      since: "2026-07-01T00:00:00Z",
      until: "2026-08-01T00:00:00Z",
      projects: [],
      sources: [],
      agents: [],
      models: [],
      bucket,
    },
    summary,
    time_series: [
      {
        start: "2026-07-31T00:00:00Z",
        end: "2026-08-01T00:00:00Z",
        ...summary,
      },
    ],
    projects: [{ project: "project-a", ...summary }],
    sources: [{ value: "ci", ...summary }],
    agents: [{ value: "agent-a", ...summary }],
    models: [{ value: "model-a", ...summary }],
    options: {
      projects: ["project-a"],
      sources: ["ci"],
      agents: ["agent-a"],
      models: ["model-a"],
    },
  };
}

function storeFor(
  snapshot: AnalyticsSnapshot | null,
  options: { stale?: boolean; error?: string | null; loading?: boolean } = {},
): AnalyticsStore {
  return {
    getFilters: () => filters,
    getSnapshot: () => snapshot,
    isLoading: () => options.loading ?? false,
    isStale: () => options.stale ?? false,
    getError: () => options.error ?? null,
    getLastUpdatedAt: () => Date.now(),
    refresh: vi.fn(async () => undefined),
    setFilters: vi.fn(async () => undefined),
    start: vi.fn(),
    dispose: vi.fn(),
  };
}

afterEach(cleanup);

describe("AnalyticsView", () => {
  it("shows verdict failures, run errors, and incomplete estimated cost", () => {
    render(AnalyticsView, { store: storeFor(makeSnapshot()) });

    expect(
      screen.getByRole("img", { name: "Review failure rate over time" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("img", { name: "Median review latency over time" }),
    ).toBeTruthy();
    expect(screen.getAllByText("22%").length).toBeGreaterThan(1);
    expect(
      screen.getAllByText("2 failed verdicts of 9 rated reviews").length,
    ).toBeGreaterThan(1);
    expect(
      screen.getByText("1 run error · 1 canceled · 1 skipped"),
    ).toBeTruthy();
    expect(screen.getAllByText("Estimated cost").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Pricing coverage").length).toBeGreaterThan(0);
    expect(screen.getByText("2 of 4 eligible attempts priced")).toBeTruthy();
    expect(screen.getByText("Estimated cost is a lower bound")).toBeTruthy();
    expect(screen.getByRole("cell", { name: "project-a" })).toBeTruthy();
    expect(screen.getByRole("row", { name: /project-a 12 22%/ })).toBeTruthy();
  });

  it("formats automatic time buckets from the server-selected bucket", () => {
    render(AnalyticsView, { store: storeFor(makeSnapshot(12, "hour")) });

    expect(screen.getByRole("button", { name: /Jul 31, .*: 12/ })).toBeTruthy();
  });

  it("marks retained results stale and retries on demand", async () => {
    const store = storeFor(makeSnapshot(), {
      stale: true,
      error: "offline",
    });
    render(AnalyticsView, {
      store,
    });

    expect(screen.getByRole("status").textContent).toContain(
      "Showing stale data",
    );
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(store.refresh).toHaveBeenCalledOnce();
  });

  it("shows errors and empty ranges without unrelated data", () => {
    const { unmount } = render(AnalyticsView, {
      store: storeFor(null, { error: "request failed" }),
    });
    expect(
      screen.getByRole("heading", { name: "Analytics unavailable" }),
    ).toBeTruthy();
    expect(screen.getByText("request failed")).toBeTruthy();
    unmount();

    const empty = makeSnapshot(0);
    empty.summary.attempts.eligible = 0;
    empty.summary.cost = {
      total_usd: 0,
      eligible_attempts: 0,
      priced_attempts: 0,
      coverage: 0,
      complete: false,
    };
    render(AnalyticsView, { store: storeFor(empty) });
    expect(screen.getByText("No reviews in this range")).toBeTruthy();
  });

  it("renders attempt and cost data when there are no logical reviews", () => {
    render(AnalyticsView, { store: storeFor(makeSnapshot(0)) });

    expect(screen.queryByText("No reviews in this range")).toBeNull();
    expect(screen.getAllByText("Estimated cost").length).toBeGreaterThan(0);
    expect(screen.getByText("2 of 4 eligible attempts priced")).toBeTruthy();
  });
});
