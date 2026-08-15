import type { RoborevClient } from "../api/client";
import type { components, operations } from "../api/generated";

export type AnalyticsSnapshot = components["schemas"]["AnalyticsSnapshot"];
export type AnalyticsQuery = NonNullable<
  operations["get-web-analytics"]["parameters"]["query"]
>;
export type AnalyticsLoader = (
  query: AnalyticsQuery,
  signal: AbortSignal,
) => Promise<AnalyticsSnapshot>;

export type AnalyticsRange = "24h" | "7d" | "30d" | "90d" | "1y" | "all";
export type AnalyticsBucket = "auto" | "hour" | "day" | "week" | "month";

export interface AnalyticsFilters {
  range: AnalyticsRange;
  projects: string[];
  sources: string[];
  agent: string;
  model: string;
  bucket: AnalyticsBucket;
}

interface AnalyticsStoreOptions {
  client?: RoborevClient;
  loader?: AnalyticsLoader;
  now?: () => Date;
}

const RANGE_MILLISECONDS: Record<Exclude<AnalyticsRange, "all">, number> = {
  "24h": 24 * 60 * 60 * 1_000,
  "7d": 7 * 24 * 60 * 60 * 1_000,
  "30d": 30 * 24 * 60 * 60 * 1_000,
  "90d": 90 * 24 * 60 * 60 * 1_000,
  "1y": 365 * 24 * 60 * 60 * 1_000,
};

const VALID_RANGES = new Set<AnalyticsRange>([
  "24h",
  "7d",
  "30d",
  "90d",
  "1y",
  "all",
]);
const VALID_BUCKETS = new Set<AnalyticsBucket>([
  "auto",
  "hour",
  "day",
  "week",
  "month",
]);
const BUCKET_ORDER: Record<Exclude<AnalyticsBucket, "auto">, number> = {
  hour: 0,
  day: 1,
  week: 2,
  month: 3,
};

export function createAnalyticsStore(options: AnalyticsStoreOptions) {
  const loader = options.loader ?? makeAnalyticsLoader(options.client);
  const now = options.now ?? (() => new Date());
  const initialFilters = readAnalyticsFilters(globalThis.location.search);
  let filters = $state(initialFilters);
  let snapshot = $state<AnalyticsSnapshot | null>(null);
  let snapshotKey = "";
  let loading = $state(false);
  let stale = $state(false);
  let error = $state<string | null>(null);
  let lastUpdatedAt = $state<number | null>(null);
  let requestGeneration = 0;
  let controller: AbortController | undefined;
  let started = false;

  writeAnalyticsFilters(initialFilters, true);

  async function refresh(): Promise<void> {
    const key = analyticsFilterKey(filters);
    const generation = ++requestGeneration;
    controller?.abort();
    controller = new AbortController();
    loading = true;
    stale = false;
    error = null;
    try {
      const result = await loader(
        analyticsRequestQuery(filters, now()),
        controller.signal,
      );
      if (
        generation !== requestGeneration ||
        key !== analyticsFilterKey(filters)
      ) {
        return;
      }
      snapshot = result;
      snapshotKey = key;
      lastUpdatedAt = Date.now();
    } catch (cause) {
      if (generation !== requestGeneration || controller.signal.aborted) return;
      error = analyticsErrorMessage(cause);
      if (snapshot !== null && snapshotKey === key) {
        stale = true;
      } else {
        snapshot = null;
        snapshotKey = "";
      }
    } finally {
      if (generation === requestGeneration) loading = false;
    }
  }

  async function setFilters(update: Partial<AnalyticsFilters>): Promise<void> {
    const next = normalizeAnalyticsFilters({ ...filters, ...update });
    const changed = analyticsFilterKey(next) !== analyticsFilterKey(filters);
    filters = next;
    writeAnalyticsFilters(filters, false);
    if (changed) {
      snapshot = null;
      snapshotKey = "";
      stale = false;
      error = null;
    }
    await refresh();
  }

  const handlePopState = (): void => {
    const next = readAnalyticsFilters(globalThis.location.search);
    const changed = analyticsFilterKey(next) !== analyticsFilterKey(filters);
    filters = next;
    writeAnalyticsFilters(filters, true);
    if (changed) {
      snapshot = null;
      snapshotKey = "";
    }
    void refresh();
  };

  const handleFocus = (): void => {
    if (globalThis.document.visibilityState === "visible") void refresh();
  };

  function start(): void {
    if (started) return;
    started = true;
    globalThis.addEventListener("popstate", handlePopState);
    globalThis.addEventListener("focus", handleFocus);
    void refresh();
  }

  function dispose(): void {
    controller?.abort();
    requestGeneration++;
    if (!started) return;
    started = false;
    globalThis.removeEventListener("popstate", handlePopState);
    globalThis.removeEventListener("focus", handleFocus);
  }

  return {
    getFilters: () => filters,
    getSnapshot: () => snapshot,
    isLoading: () => loading,
    isStale: () => stale,
    getError: () => error,
    getLastUpdatedAt: () => lastUpdatedAt,
    refresh,
    setFilters,
    start,
    dispose,
  };
}

function makeAnalyticsLoader(
  client: RoborevClient | undefined,
): AnalyticsLoader {
  if (client === undefined) {
    throw new Error("analytics store requires a client or loader");
  }
  return async (query, signal) => {
    const result = await client.GET("/api/ui/analytics", {
      params: { query },
      signal,
    });
    if (result.data !== undefined) return result.data;
    const detail =
      result.error && "detail" in result.error
        ? result.error.detail
        : undefined;
    throw new Error(detail ?? "Analytics request failed");
  };
}

export function readAnalyticsFilters(search: string): AnalyticsFilters {
  const params = new URLSearchParams(search);
  const rangeValue = params.get("range") ?? "30d";
  const bucketValue = params.get("bucket") ?? "auto";
  return normalizeAnalyticsFilters({
    range: VALID_RANGES.has(rangeValue as AnalyticsRange)
      ? (rangeValue as AnalyticsRange)
      : "30d",
    projects: params.getAll("project"),
    sources: params.getAll("source"),
    agent: params.get("agent") ?? "",
    model: params.get("model") ?? "",
    bucket: VALID_BUCKETS.has(bucketValue as AnalyticsBucket)
      ? (bucketValue as AnalyticsBucket)
      : "auto",
  });
}

function normalizeAnalyticsFilters(
  filters: AnalyticsFilters,
): AnalyticsFilters {
  const range = VALID_RANGES.has(filters.range) ? filters.range : "30d";
  const bucket = VALID_BUCKETS.has(filters.bucket) ? filters.bucket : "auto";
  return {
    range,
    projects: sortedUniqueNonempty(filters.projects),
    // The daemon's stored value for manually queued reviews is the empty
    // string. Preserve it as an explicit filter instead of treating it as an
    // absent selection.
    sources: sortedUnique(filters.sources),
    agent: filters.agent.trim(),
    model: filters.model.trim(),
    bucket: compatibleAnalyticsBucket(range, bucket),
  };
}

function compatibleAnalyticsBucket(
  range: AnalyticsRange,
  bucket: AnalyticsBucket,
): AnalyticsBucket {
  if (bucket === "auto") return bucket;
  const minimum =
    range === "all"
      ? "month"
      : range === "90d" || range === "1y"
        ? "day"
        : "hour";
  return BUCKET_ORDER[bucket] < BUCKET_ORDER[minimum] ? minimum : bucket;
}

function writeAnalyticsFilters(
  filters: AnalyticsFilters,
  replace: boolean,
): void {
  const params = new URLSearchParams();
  params.set("range", filters.range);
  for (const project of filters.projects) params.append("project", project);
  for (const source of filters.sources) params.append("source", source);
  if (filters.agent !== "") params.set("agent", filters.agent);
  if (filters.model !== "") params.set("model", filters.model);
  if (filters.bucket !== "auto") params.set("bucket", filters.bucket);
  const target = `${globalThis.location.pathname}?${params.toString()}`;
  if (replace) globalThis.history.replaceState(null, "", target);
  else globalThis.history.pushState(null, "", target);
}

function analyticsRequestQuery(
  filters: AnalyticsFilters,
  requestTime: Date,
): AnalyticsQuery {
  const until = requestTime.toISOString();
  const query: AnalyticsQuery = {
    until,
    project: filters.projects,
    source: filters.sources,
  };
  if (filters.range !== "all") {
    query.since = new Date(
      requestTime.getTime() - RANGE_MILLISECONDS[filters.range],
    ).toISOString();
  }
  if (filters.agent !== "") query.agent = filters.agent;
  if (filters.model !== "") query.model = filters.model;
  if (filters.bucket !== "auto") query.bucket = filters.bucket;
  return query;
}

function analyticsFilterKey(filters: AnalyticsFilters): string {
  return JSON.stringify(filters);
}

function sortedUniqueNonempty(values: string[]): string[] {
  return sortedUnique(values).filter((value) => value !== "");
}

function sortedUnique(values: string[]): string[] {
  return Array.from(new Set(values.map((value) => value.trim()))).sort();
}

function analyticsErrorMessage(cause: unknown): string {
  if (cause instanceof Error && cause.message !== "") return cause.message;
  return "Analytics request failed";
}

export type AnalyticsStore = ReturnType<typeof createAnalyticsStore>;
