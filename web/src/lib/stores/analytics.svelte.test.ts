import { afterEach, describe, expect, it, vi } from "vitest";

import type { components } from "../api/generated";
import { createAnalyticsStore, type AnalyticsLoader } from "./analytics.svelte";

type AnalyticsSnapshot = components["schemas"]["AnalyticsSnapshot"];

function snapshot(total: number): AnalyticsSnapshot {
  const percentiles = { p50_secs: 0, p90_secs: 0, p99_secs: 0 };
  const reviews = {
    total,
    done: total,
    failed: 0,
    canceled: 0,
    skipped: 0,
    failure_rate: 0,
    run_errors: 0,
    run_error_rate: 0,
  };
  const attempts = { eligible: total, duration: percentiles };
  const cost = {
    total_usd: total,
    eligible_attempts: total,
    priced_attempts: total,
    coverage: total > 0 ? 1 : 0,
    complete: total > 0,
  };
  return {
    schema_version: 1,
    filters: {
      since: "2026-08-01T00:00:00Z",
      until: "2026-08-02T00:00:00Z",
      projects: [],
      sources: [],
      agents: [],
      models: [],
      bucket: "day",
    },
    summary: {
      reviews,
      verdicts: {
        passed: total,
        fail_open: 0,
        fail_closed: 0,
        rated: total,
        failure_rate: 0,
      },
      review_latency: percentiles,
      attempts,
      cost,
    },
    time_series: [],
    projects: [],
    sources: [],
    agents: [],
    models: [],
    options: { projects: [], sources: [], agents: [], models: [] },
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

afterEach(() => {
  history.replaceState(null, "", "/analytics");
  document.head.querySelector('meta[name="roborev-base-path"]')?.remove();
});

describe("analytics store URL filters", () => {
  it("keeps filter history below the configured prefix", async () => {
    const meta = document.createElement("meta");
    meta.name = "roborev-base-path";
    meta.content = "/roborev-ci";
    document.head.append(meta);
    history.replaceState(null, "", "/roborev-ci/analytics");
    const store = createAnalyticsStore({
      loader: vi.fn(async () => snapshot(0)),
    });

    await store.setFilters({ range: "7d" });

    expect(location.pathname).toBe("/roborev-ci/analytics");
    store.dispose();
  });

  it("normalizes supported filters and removes unrelated query values", () => {
    history.replaceState(
      null,
      "",
      "/analytics?range=7d&project=zeta&project=alpha&project=zeta&source=ci&bucket=day&junk=x",
    );
    const store = createAnalyticsStore({
      loader: vi.fn(async () => snapshot(0)),
    });

    expect(store.getFilters()).toEqual({
      range: "7d",
      projects: ["alpha", "zeta"],
      sources: ["ci"],
      agent: "",
      model: "",
      bucket: "day",
    });
    expect(location.search).toBe(
      "?range=7d&project=alpha&project=zeta&source=ci&bucket=day",
    );
    store.dispose();
  });

  it("keeps Manual as an explicit source in the URL and request", async () => {
    const loader = vi.fn<AnalyticsLoader>().mockResolvedValue(snapshot(1));
    const store = createAnalyticsStore({
      loader,
      now: () => new Date("2026-08-13T20:00:00Z"),
    });

    await store.setFilters({ sources: [""] });

    expect(store.getFilters().sources).toEqual([""]);
    expect(new URLSearchParams(location.search).getAll("source")).toEqual([""]);
    expect(loader).toHaveBeenLastCalledWith(
      expect.objectContaining({ source: [""] }),
      expect.any(AbortSignal),
    );
    store.dispose();
  });

  it.each([
    { range: "90d" as const, bucket: "hour" as const, expected: "day" },
    { range: "1y" as const, bucket: "hour" as const, expected: "day" },
    { range: "all" as const, bucket: "week" as const, expected: "month" },
  ])(
    "widens $range/$bucket requests to $expected buckets",
    async ({ range, bucket, expected }) => {
      const loader = vi.fn<AnalyticsLoader>().mockResolvedValue(snapshot(1));
      const store = createAnalyticsStore({ loader });

      await store.setFilters({ range, bucket });

      expect(store.getFilters().bucket).toBe(expected);
      expect(loader).toHaveBeenLastCalledWith(
        expect.objectContaining({ bucket: expected }),
        expect.any(AbortSignal),
      );
      expect(new URLSearchParams(location.search).get("bucket")).toBe(expected);
      store.dispose();
    },
  );
});

describe("analytics store request ownership", () => {
  it("aborts obsolete requests and ignores their late responses", async () => {
    const first = deferred<AnalyticsSnapshot>();
    const second = deferred<AnalyticsSnapshot>();
    const signals: AbortSignal[] = [];
    const loader: AnalyticsLoader = vi
      .fn()
      .mockImplementationOnce((_query, signal) => {
        signals.push(signal);
        return first.promise;
      })
      .mockImplementationOnce((_query, signal) => {
        signals.push(signal);
        return second.promise;
      });
    const store = createAnalyticsStore({ loader });

    const oldRequest = store.refresh();
    const newRequest = store.setFilters({ range: "7d" });
    expect(signals[0]?.aborted).toBe(true);
    second.resolve(snapshot(7));
    await newRequest;
    first.resolve(snapshot(30));
    await oldRequest;

    expect(store.getSnapshot()?.summary.reviews.total).toBe(7);
    expect(store.getFilters().range).toBe("7d");
    store.dispose();
  });

  it("retains a same-query snapshot as stale when refresh fails", async () => {
    const loader = vi
      .fn<AnalyticsLoader>()
      .mockResolvedValueOnce(snapshot(4))
      .mockRejectedValueOnce(new Error("offline"));
    const store = createAnalyticsStore({ loader });

    await store.refresh();
    await store.refresh();

    expect(store.getSnapshot()?.summary.reviews.total).toBe(4);
    expect(store.isStale()).toBe(true);
    expect(store.getError()).toBe("offline");
    store.dispose();
  });

  it("never displays the old snapshot under failed new filters", async () => {
    const loader = vi
      .fn<AnalyticsLoader>()
      .mockResolvedValueOnce(snapshot(30))
      .mockRejectedValueOnce(new Error("request failed"));
    const store = createAnalyticsStore({ loader });

    await store.refresh();
    await store.setFilters({ range: "24h" });

    expect(store.getSnapshot()).toBeNull();
    expect(store.isStale()).toBe(false);
    expect(store.getError()).toBe("request failed");
    store.dispose();
  });

  it("refreshes when the browser regains focus", async () => {
    const loader = vi.fn<AnalyticsLoader>().mockResolvedValue(snapshot(2));
    const store = createAnalyticsStore({ loader });

    store.start();
    await vi.waitFor(() => expect(loader).toHaveBeenCalledTimes(1));
    window.dispatchEvent(new Event("focus"));
    await vi.waitFor(() => expect(loader).toHaveBeenCalledTimes(2));

    store.dispose();
  });
});
