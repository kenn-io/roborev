import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { OwnedAppRuntime } from "../../runtime/runtime";
import { makeTestAppRuntime } from "../../testing/runtime";
import {
  createJobsStore as createRuntimeJobsStore,
  type JobsStore,
  type JobsStoreOptions,
} from "./jobs.svelte";
import type { ReviewJob } from "../../api/generated/models";
const actionReports = vi.hoisted(() => vi.fn());
vi.mock("../../utils/app-opened", () => ({ reportCoreAction: actionReports }));

const originalFetch = globalThis.fetch;
const runtimes = new Set<OwnedAppRuntime>();
const stores = new Set<JobsStore>();
const storeRuntimes = new WeakMap<JobsStore, OwnedAppRuntime>();
let ownerSequence = 0;

function fetchSignal(
  input: RequestInfo | URL,
  init?: RequestInit,
): AbortSignal | undefined {
  return input instanceof Request
    ? (input.signal ?? undefined)
    : (init?.signal ?? undefined);
}

function createJobsStore(options: Omit<JobsStoreOptions, "runtime" | "owner">) {
  const runtime = makeTestAppRuntime();
  runtimes.add(runtime);
  ownerSequence += 1;
  const store = createRuntimeJobsStore({
    ...options,
    runtime,
    owner: `jobs-test:${ownerSequence}`,
  });
  stores.add(store);
  storeRuntimes.set(store, runtime);
  return store;
}

async function loadJobs(store: JobsStore): Promise<void> {
  const runtime = storeRuntimes.get(store);
  if (runtime === undefined) throw new Error("test jobs store has no runtime");
  const execution = runtime.runCommand(store.loadJobsEffect(), {
    operation: "test load Roborev jobs",
    safeContext: {},
    onFailure: () => {},
  });
  const exit = await Effect.runPromise(execution.await);
  expect(exit._tag).toBe("Success");
}

async function loadMoreJobs(store: JobsStore): Promise<void> {
  const runtime = storeRuntimes.get(store);
  if (runtime === undefined) throw new Error("test jobs store has no runtime");
  const execution = runtime.runCommand(store.loadMoreEffect(), {
    operation: "test load more Roborev jobs",
    safeContext: {},
    onFailure: () => {},
  });
  const exit = await Effect.runPromise(execution.await);
  expect(exit._tag).toBe("Success");
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(async () => {
  for (const store of stores) store.dispose();
  stores.clear();
  await Promise.all(
    Array.from(runtimes, (runtime) => Effect.runPromise(runtime.disposeEffect)),
  );
  runtimes.clear();
  localStorage.clear();
});

function makeJob(
  id: number,
  startedAt?: string,
  finishedAt?: string,
): ReviewJob {
  return {
    id,
    agent: "codex",
    agentic: false,
    enqueued_at: "2026-04-11T11:00:00Z",
    git_ref: `deadbeef${id}`,
    job_type: "review",
    prompt_prebuilt: false,
    repo_id: 1,
    retry_count: 0,
    status: "done",
    ...(startedAt ? { started_at: startedAt } : {}),
    ...(finishedAt ? { finished_at: finishedAt } : {}),
  };
}

function stubEventStream() {
  let events: ReadableStreamDefaultController<Uint8Array> | undefined;
  globalThis.fetch = vi.fn((input, init) => {
    const url = input instanceof Request ? input.url : String(input);
    if (url.endsWith("/api/stream/events")) {
      return Promise.resolve(
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              events = controller;
            },
          }),
          { status: 200 },
        ),
      );
    }
    return originalFetch(input, init);
  });
  return (chunk: string) => events?.enqueue(new TextEncoder().encode(chunk));
}

describe("submitted search telemetry", () => {
  beforeEach(() => {
    actionReports.mockReset();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it("reports each submitted empty search, while refreshes stay silent", async () => {
    const api = { listJobs: vi.fn().mockResolvedValue({ jobs: [] }) };
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    store.submitSearch("feature/search");
    await vi.waitFor(() =>
      expect(actionReports).toHaveBeenCalledExactlyOnceWith("search_empty"),
    );
    store.submitSearch("feature/search");
    await vi.waitFor(() => expect(actionReports).toHaveBeenCalledTimes(2));
    expect(actionReports).toHaveBeenLastCalledWith("search_empty");
    await loadJobs(store);
    expect(actionReports).toHaveBeenCalledTimes(2);
  });

  it.each([
    ["populated", "feature/search"],
    ["empty query", ""],
    ["empty query", " \t\n"],
    ["preview", "feature/search"],
    ["failed", "feature/search"],
  ])("keeps a %s search silent", async (kind, value) => {
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: kind === "populated" ? [makeJob(1)] : [],
      }),
    };
    if (kind === "failed")
      api.listJobs.mockRejectedValue(new Error("request failed"));
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    if (kind === "preview") store.setFilter("search", "feature/search");
    else store.submitSearch(value);
    await vi.waitFor(() => {
      expect(api.listJobs).toHaveBeenCalledTimes(1);
      expect(store.isLoading()).toBe(false);
    });
    if (kind === "failed") expect(store.getError()).not.toBeNull();
    if (kind === "populated") expect(store.getJobs()).toEqual([makeJob(1)]);
    expect(actionReports).not.toHaveBeenCalled();
  });

  it("keeps a discarded empty reply silent when a live refresh shows jobs", async () => {
    const emitEvent = stubEventStream();
    const pending = Promise.withResolvers<{ jobs: ReviewJob[] }>();
    const api = {
      listJobs: vi
        .fn()
        .mockResolvedValueOnce({ jobs: [makeJob(1)] })
        .mockReturnValueOnce(pending.promise)
        .mockResolvedValue({ jobs: [makeJob(2)] }),
    };
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));
    const execution = store.submitSearch("feature/search");
    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalledTimes(2));
    emitEvent(
      '{"type":"review.completed","ts":"2026-08-04T13:00:00Z","job_id":42}\n',
    );
    await vi.waitFor(() => expect(store.getJobs()).toEqual([makeJob(2)]));
    pending.resolve({ jobs: [] });
    await Effect.runPromise(execution.await);
    expect(store.getJobs()).toEqual([makeJob(2)]);
    expect(actionReports).not.toHaveBeenCalled();
    store.disconnectEventStream(eventOwner);
  });
});

describe("createJobsStore filter preferences", () => {
  it("maps the branchless display sentinel to the empty branch query", async () => {
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: {
          queued: 1,
          running: 0,
          done: 0,
          failed: 0,
          closed: 0,
          open: 0,
        },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    store.setFilter("showAutoDesign", true);
    store.setFilter("branch", "(none)");
    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalled());

    expect(api.listJobs.mock.calls.at(-1)?.[0]).toEqual(
      expect.objectContaining({ branch_empty: "true", limit: 50 }),
    );
    expect(api.listJobs.mock.calls.at(-1)?.[0]).not.toHaveProperty("branch");
    expect(store.usesFilteredStatusCounts()).toBe(true);
    expect(store.getFilteredStatusCounts()).toEqual({
      queued: 1,
      running: 0,
      done: 0,
      failed: 0,
    });
  });

  it("restores filter choices in a new store and applies them to the jobs query", async () => {
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const firstStore = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    firstStore.setFilter("hideClosed", true);
    firstStore.setFilter("showAutoDesign", true);
    firstStore.dispose();

    const restoredStore = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    await loadJobs(restoredStore);

    expect(restoredStore.getFilterHideClosed()).toBe(true);
    expect(restoredStore.getFilterShowAutoDesign()).toBe(true);
    expect(api.listJobs).toHaveBeenCalledWith(
      {
        closed: "false",
        limit: 50,
        omit_prompt: "true",
        sort: "enqueued_at",
        direction: "desc",
      },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
  });

  it("synchronizes filter changes across live jobs stores", async () => {
    const firstApi = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const secondApi = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const firstStore = createJobsStore({
      api: firstApi as never,
      navigate: vi.fn(),
    });
    const secondStore = createJobsStore({
      api: secondApi as never,
      navigate: vi.fn(),
    });

    firstStore.setFilter("hideClosed", true);
    firstStore.setFilter("showAutoDesign", true);

    expect(secondStore.getFilterHideClosed()).toBe(true);
    expect(secondStore.getFilterShowAutoDesign()).toBe(true);
    await vi.waitFor(() =>
      expect(secondApi.listJobs).toHaveBeenCalledWith(
        {
          closed: "false",
          limit: 50,
          omit_prompt: "true",
          sort: "enqueued_at",
          direction: "desc",
        },
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
  });

  it("keeps filtering when browser storage is unavailable", async () => {
    const getItem = vi
      .spyOn(Storage.prototype, "getItem")
      .mockImplementation(() => {
        throw new Error("storage unavailable");
      });
    const firstApi = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const secondApi = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const setItem = vi
      .spyOn(Storage.prototype, "setItem")
      .mockImplementation(() => {
        throw new Error("storage unavailable");
      });

    try {
      const firstStore = createJobsStore({
        api: firstApi as never,
        navigate: vi.fn(),
      });
      expect(firstStore.getFilterHideClosed()).toBe(false);
      expect(firstStore.getFilterShowAutoDesign()).toBe(false);

      firstStore.setFilter("hideClosed", true);
      firstStore.setFilter("showAutoDesign", true);
      const secondStore = createJobsStore({
        api: secondApi as never,
        navigate: vi.fn(),
      });
      await loadJobs(secondStore);

      expect(secondStore.getFilterHideClosed()).toBe(true);
      expect(secondStore.getFilterShowAutoDesign()).toBe(true);
      expect(secondApi.listJobs).toHaveBeenCalledWith(
        {
          closed: "false",
          limit: 50,
          omit_prompt: "true",
          sort: "enqueued_at",
          direction: "desc",
        },
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      );
    } finally {
      getItem.mockRestore();
      setItem.mockRestore();
    }
  });
});

describe("createJobsStore sorting", () => {
  function costJob(id: number, costUsd: number): ReviewJob {
    return {
      ...makeJob(id),
      token_usage: JSON.stringify({ has_cost: true, cost_usd: costUsd }),
    };
  }

  function sortedApi() {
    // Each sort serves two pages linked by a cursor that names the sort, so a
    // request that mixes one sort's cursor with another sort is visible. Like
    // the daemon, each page is in sort order and the second page resumes
    // strictly after the first.
    const pages: Record<string, ReviewJob[][]> = {
      "enqueued_at:desc": [
        [makeJob(30), makeJob(29)],
        [makeJob(28), makeJob(27)],
      ],
      "id:desc": [
        [makeJob(9), makeJob(7), makeJob(5)],
        [makeJob(2), makeJob(1)],
      ],
      "id:asc": [
        [makeJob(1), makeJob(2), makeJob(5)],
        [makeJob(7), makeJob(9)],
      ],
      // Highest cost first, so the IDs are out of numeric order.
      "cost:desc": [
        [costJob(3, 0.9), costJob(8, 0.5)],
        [costJob(1, 0.4), costJob(6, 0.1)],
      ],
    };
    return {
      listJobs: vi.fn().mockImplementation(async (query) => {
        const typed = query as {
          sort?: string;
          direction?: string;
          cursor?: string;
        };
        const key = `${typed.sort}:${typed.direction}`;
        const sortPages = pages[key];
        if (sortPages === undefined) throw new Error(`unexpected sort ${key}`);
        const second = typed.cursor === `${key}:1`;
        if (typed.cursor !== undefined && !second) {
          throw new Error(`cursor ${typed.cursor} does not match ${key}`);
        }
        return {
          jobs: sortPages[second ? 1 : 0],
          has_more: !second,
          next_cursor: second ? null : `${key}:1`,
          stats: { done: 4, closed: 0, open: 4 },
        };
      }),
    };
  }

  function ids(store: JobsStore): number[] {
    return store.getJobs().map((job) => job.id);
  }

  it("reloads the first page with the new sort when the sort changes", async () => {
    const api = sortedApi();
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    await loadJobs(store);
    await loadMoreJobs(store);
    expect(ids(store)).toEqual([30, 29, 28, 27]);

    store.setSortColumn("id");

    await vi.waitFor(() => expect(ids(store)).toEqual([9, 7, 5]));
    const reload = api.listJobs.mock.calls.at(-1)?.[0];
    expect(reload).toMatchObject({ sort: "id", direction: "desc", limit: 50 });
    expect(reload.cursor).toBeUndefined();

    store.setSortColumn("id");

    await vi.waitFor(() => expect(ids(store)).toEqual([1, 2, 5]));
    expect(api.listJobs.mock.calls.at(-1)?.[0]).toMatchObject({
      sort: "id",
      direction: "asc",
    });
  });

  it("opens text columns ascending and numeric columns descending", async () => {
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [makeJob(1)],
        has_more: false,
        stats: { done: 1, closed: 0, open: 1 },
      }),
    };
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    await loadJobs(store);
    expect(api.listJobs.mock.calls[0]?.[0]).toMatchObject({
      sort: "enqueued_at",
      direction: "desc",
    });

    store.setSortColumn("agent");
    await vi.waitFor(() =>
      expect(api.listJobs.mock.calls.at(-1)?.[0]).toMatchObject({
        sort: "agent",
        direction: "asc",
      }),
    );

    store.setSortColumn("cost");
    await vi.waitFor(() =>
      expect(api.listJobs.mock.calls.at(-1)?.[0]).toMatchObject({
        sort: "cost",
        direction: "desc",
      }),
    );
  });

  it("appends the next page in the daemon's order with the current sort", async () => {
    const api = sortedApi();
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    await loadJobs(store);
    store.setSortColumn("cost");
    await vi.waitFor(() => expect(ids(store)).toEqual([3, 8]));

    await loadMoreJobs(store);

    expect(api.listJobs.mock.calls.at(-1)?.[0]).toMatchObject({
      sort: "cost",
      direction: "desc",
      cursor: "cost:desc:1",
      limit: 50,
    });
    expect(store.getLoadMoreError()).toBeNull();
    expect(ids(store)).toEqual([3, 8, 1, 6]);
    expect(store.getHasMore()).toBe(false);
  });

  it("drops a page that was loading for the previous sort", async () => {
    const sorted = sortedApi();
    const stalled = Promise.withResolvers<void>();
    const api = {
      listJobs: vi.fn().mockImplementation(async (query) => {
        if ((query as { cursor?: string }).cursor !== undefined) {
          await stalled.promise;
        }
        return sorted.listJobs(query);
      }),
    };
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    await loadJobs(store);
    store.loadMore();
    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalledTimes(2));

    store.setSortColumn("id");
    await vi.waitFor(() => expect(ids(store)).toEqual([9, 7, 5]));
    stalled.resolve();
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));

    expect(ids(store)).toEqual([9, 7, 5]);
    expect(store.getHasMore()).toBe(true);
  });

  it("shows the new sort when a rerun finishes during the sort reload", async () => {
    const sorted = sortedApi();
    const firstSortReply = Promise.withResolvers<void>();
    const rerunReply = Promise.withResolvers<{ job_id: number }>();
    let sortRequests = 0;
    const api = {
      listJobs: vi.fn().mockImplementation(async (query) => {
        const typed = query as { id?: number; sort?: string };
        if (typed.id !== undefined) {
          return {
            jobs: [makeJob(typed.id)],
            has_more: false,
            stats: { done: 1, closed: 0, open: 1 },
          };
        }
        if (typed.sort === "id" && ++sortRequests === 1) {
          await firstSortReply.promise;
        }
        return sorted.listJobs(query);
      }),
      rerunJob: vi.fn().mockReturnValue(rerunReply.promise),
    };
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    await loadJobs(store);
    store.rerunJob(30);
    await vi.waitFor(() => expect(api.rerunJob).toHaveBeenCalledOnce());

    // The sort reload is still loading when the rerun's own reload starts.
    store.setSortColumn("id");
    rerunReply.resolve({ job_id: 30 });

    await vi.waitFor(() => expect(store.isSortPending()).toBe(false));
    expect(ids(store)).toEqual([9, 7, 5]);
    firstSortReply.resolve();
  });

  it("reports a failed sort when a rerun's reload fails during the sort reload", async () => {
    const sorted = sortedApi();
    const firstSortReply = Promise.withResolvers<void>();
    const rerunReply = Promise.withResolvers<{ job_id: number }>();
    let sortRequests = 0;
    const api = {
      listJobs: vi.fn().mockImplementation(async (query) => {
        const typed = query as { id?: number; sort?: string };
        if (typed.id !== undefined) {
          return {
            jobs: [makeJob(typed.id)],
            has_more: false,
            stats: { done: 1, closed: 0, open: 1 },
          };
        }
        if (typed.sort === "id") {
          sortRequests += 1;
          if (sortRequests === 1) await firstSortReply.promise;
          else throw new Error("daemon unavailable");
        }
        return sorted.listJobs(query);
      }),
      rerunJob: vi.fn().mockReturnValue(rerunReply.promise),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
      onError: vi.fn(),
    });
    await loadJobs(store);
    store.rerunJob(30);
    await vi.waitFor(() => expect(api.rerunJob).toHaveBeenCalledOnce());

    // The rerun's reload replaces the sort reload, then fails.
    store.setSortColumn("id");
    rerunReply.resolve({ job_id: 30 });

    await vi.waitFor(() =>
      expect(store.getSortError()).toEqual(expect.any(String)),
    );
    expect(store.isSortPending()).toBe(false);
    firstSortReply.resolve();
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));
    expect(ids(store)).toEqual([30, 29]);
  });

  it.each([
    { reload: "succeeds", sortError: null, rows: [9, 7, 5] },
    { reload: "fails", sortError: expect.any(String), rows: [30, 29] },
  ])(
    "settles the sort when a rerun's job lookup fails and its reload $reload",
    async ({ reload, sortError, rows }) => {
      const sorted = sortedApi();
      const firstSortReply = Promise.withResolvers<void>();
      const rerunReload = Promise.withResolvers<void>();
      const rerunReply = Promise.withResolvers<{ job_id: number }>();
      let sortRequests = 0;
      const api = {
        listJobs: vi.fn().mockImplementation(async (query) => {
          const typed = query as { id?: number; sort?: string };
          if (typed.id !== undefined) throw new Error("lookup failed");
          if (typed.sort === "id") {
            sortRequests += 1;
            if (sortRequests === 1) {
              await firstSortReply.promise;
            } else {
              await rerunReload.promise;
              if (reload === "fails") throw new Error("daemon unavailable");
            }
          }
          return sorted.listJobs(query);
        }),
        rerunJob: vi.fn().mockReturnValue(rerunReply.promise),
      };
      const onError = vi.fn();
      const store = createJobsStore({
        api: api as never,
        navigate: vi.fn(),
        onError,
      });
      await loadJobs(store);
      store.rerunJob(30);
      await vi.waitFor(() => expect(api.rerunJob).toHaveBeenCalledOnce());

      // The rerun's reload replaces the sort reload, and its job lookup
      // fails before the reload returns.
      store.setSortColumn("id");
      rerunReply.resolve({ job_id: 30 });
      await vi.waitFor(() => expect(sortRequests).toBe(2));
      await vi.waitFor(() =>
        expect(
          api.listJobs.mock.calls.some(([query]) => query.id !== undefined),
        ).toBe(true),
      );
      rerunReload.resolve();

      await vi.waitFor(() => expect(store.isSortPending()).toBe(false));
      expect(store.getSortError()).toEqual(sortError);
      expect(ids(store)).toEqual(rows);
      await vi.waitFor(() => expect(onError).toHaveBeenCalled());
      firstSortReply.resolve();
    },
  );

  it("marks the sort pending until rows in the new order arrive", async () => {
    const sorted = sortedApi();
    const reply = Promise.withResolvers<void>();
    const api = {
      listJobs: vi.fn().mockImplementation(async (query) => {
        if ((query as { sort?: string }).sort === "id") await reply.promise;
        return sorted.listJobs(query);
      }),
    };
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    await loadJobs(store);
    expect(store.isSortPending()).toBe(false);

    store.setSortColumn("id");

    expect(store.isSortPending()).toBe(true);
    expect(ids(store)).toEqual([30, 29]);
    reply.resolve();
    await vi.waitFor(() => expect(ids(store)).toEqual([9, 7, 5]));
    expect(store.isSortPending()).toBe(false);
    expect(store.getSortError()).toBeNull();
  });

  it("reports a failed sort reload apart from other load failures", async () => {
    const sorted = sortedApi();
    let failSort = true;
    const api = {
      listJobs: vi.fn().mockImplementation(async (query) => {
        if ((query as { sort?: string }).sort === "id" && failSort) {
          throw new Error("daemon unavailable");
        }
        return sorted.listJobs(query);
      }),
    };
    const store = createJobsStore({ api: api as never, navigate: vi.fn() });
    await loadJobs(store);

    store.setSortColumn("id");

    await vi.waitFor(() =>
      expect(store.getSortError()).toEqual(expect.any(String)),
    );
    expect(store.isSortPending()).toBe(false);
    expect(store.getError()).toBeNull();
    expect(ids(store)).toEqual([30, 29]);

    failSort = false;
    await loadJobs(store);

    expect(ids(store)).toEqual([9, 7, 5]);
    expect(store.getSortError()).toBeNull();
    expect(store.getError()).toBeNull();
  });
});

describe("createJobsStore event stream", () => {
  type JobsResponse = {
    jobs: ReviewJob[];
    has_more: boolean;
    next_cursor?: string;
    stats: { done: number; running: number; closed: number; open: number };
  };
  const ids = (from: number, to: number) =>
    Array.from({ length: from - to + 1 }, (_, index) => from - index);

  afterEach(() => {
    globalThis.fetch = originalFetch;
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it("aborts a connection that is still waiting for response headers", async () => {
    let signal: AbortSignal | undefined;
    const fetchMock = vi.fn(
      (_input: RequestInfo | URL, init?: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          signal = fetchSignal(_input, init);
          signal?.addEventListener("abort", () => {
            reject(new DOMException("Aborted", "AbortError"));
          });
        }),
    );
    globalThis.fetch = fetchMock;
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(signal).toBeDefined());

    store.disconnectEventStream(eventOwner);

    expect(signal?.aborted).toBe(true);
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(false));
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("does not let stale event teardown disconnect a successor lease", async () => {
    const signals: AbortSignal[] = [];
    globalThis.fetch = vi.fn(
      async (input: RequestInfo | URL, init?: RequestInit) => {
        const signal = fetchSignal(input, init);
        if (signal !== undefined) signals.push(signal);
        return new Response(new ReadableStream<Uint8Array>(), { status: 200 });
      },
    );
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    const firstLease = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(signals).toHaveLength(1));
    const secondLease = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(signals).toHaveLength(2));

    const testRuntime = storeRuntimes.get(store);
    if (testRuntime === undefined)
      throw new Error("test jobs store has no runtime");
    const staleTeardown = testRuntime.runCommand(
      store.disconnectEventStreamEffect(firstLease),
      {
        operation: "disconnect stale Roborev event lease",
        safeContext: {},
        onFailure: () => {},
      },
    );
    await Effect.runPromise(staleTeardown.await);

    expect(signals[0]?.aborted).toBe(true);
    expect(signals[1]?.aborted).toBe(false);
    expect(store.isEventStreamConnected()).toBe(true);
    store.disconnectEventStream(secondLease);
  });

  it("parses split NDJSON events and cancels an active response body", async () => {
    const encoder = new TextEncoder();
    let bodyController: ReadableStreamDefaultController<Uint8Array> | undefined;
    let bodyCancelled = false;
    let signal: AbortSignal | undefined;
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        bodyController = controller;
      },
      cancel() {
        bodyCancelled = true;
      },
    });
    globalThis.fetch = vi.fn(
      async (_input: RequestInfo | URL, init?: RequestInit) => {
        signal = fetchSignal(_input, init);
        return new Response(body, {
          status: 200,
          headers: { "Content-Type": "application/x-ndjson" },
        });
      },
    );
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));
    expect(api.listJobs).toHaveBeenCalledTimes(1);
    bodyController?.enqueue(encoder.encode('{"type":"review.com'));
    bodyController?.enqueue(
      encoder.encode(
        'pleted","ts":"2026-08-04T13:00:00Z","job_id":42,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n',
      ),
    );

    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalledTimes(2));
    store.disconnectEventStream(eventOwner);

    await vi.waitFor(() => expect(bodyCancelled).toBe(true));
    await vi.waitFor(() => expect(signal?.aborted).toBe(true));
    expect(store.isEventStreamConnected()).toBe(false);
  });

  it("keeps event reconciliation alive across overlapping list requests", async () => {
    const emitChunk = stubEventStream();
    let requestCount = 0;
    let firstSignal: AbortSignal | undefined;
    let resolveFirst:
      | ((value: {
          jobs: never[];
          has_more: boolean;
          stats: { done: number; closed: number; open: number };
        }) => void)
      | undefined;
    const api = {
      listJobs: vi.fn(
        (_query: unknown, options: { signal?: AbortSignal } | undefined) => {
          requestCount += 1;
          if (requestCount !== 2) {
            return Promise.resolve({
              jobs: [],
              has_more: false,
              stats: { done: 0, closed: 0, open: 0 },
            });
          }
          return new Promise((resolve, reject) => {
            firstSignal = options?.signal;
            resolveFirst = resolve;
            options?.signal?.addEventListener("abort", () => {
              reject(new DOMException("Aborted", "AbortError"));
            });
          });
        },
      ),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    store.setSelectedJobId(42);
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));
    expect(api.listJobs).toHaveBeenCalledTimes(1);

    emitChunk(
      '{"type":"review.completed","ts":"2026-08-04T13:00:00Z","job_id":42}\n',
    );
    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalledTimes(2));
    await loadJobs(store);
    expect(api.listJobs).toHaveBeenCalledTimes(3);
    expect(firstSignal?.aborted).toBe(false);
    resolveFirst?.({
      jobs: [],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });

    emitChunk(
      '{"type":"review.closed","ts":"2026-08-04T13:01:00Z","job_id":42}\n',
    );
    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalledTimes(4));

    expect(store.isEventStreamConnected()).toBe(true);
    store.disconnectEventStream(eventOwner);
  });

  it("drains burst events while coalescing a slow reconciliation", async () => {
    const emitChunk = stubEventStream();

    const pending: Array<(value: JobsResponse) => void> = [];
    let requestCount = 0;
    const api = {
      listJobs: vi.fn(() => {
        requestCount += 1;
        if (requestCount === 1) {
          return Promise.resolve({
            jobs: [],
            has_more: false,
            stats: { done: 0, running: 0, closed: 0, open: 0 },
          });
        }
        return new Promise<JobsResponse>((resolve) => pending.push(resolve));
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    store.setSelectedJobId(42);
    const initialRevision = store.getSelectedReviewRevision();
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));

    const emitEvent = (index: number) => {
      emitChunk(
        `{"type":"review.completed","ts":"2026-08-04T13:00:${String(index).padStart(2, "0")}Z","job_id":42,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n`,
      );
    };
    emitEvent(0);
    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalledTimes(2));
    for (let index = 1; index < 12; index += 1) {
      emitEvent(index);
    }

    await vi.waitFor(() =>
      expect(store.getSelectedReviewRevision()).toBe(initialRevision + 12),
    );
    pending[0]?.({
      jobs: [{ ...makeJob(42), status: "running" }],
      has_more: false,
      stats: { done: 0, running: 1, closed: 0, open: 1 },
    });
    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalledTimes(3));
    pending[1]?.({
      jobs: [{ ...makeJob(42), status: "done" }],
      has_more: false,
      stats: { done: 1, running: 0, closed: 0, open: 1 },
    });
    await vi.waitFor(() => expect(store.getJobs()[0]?.status).toBe("done"));
    store.disconnectEventStream(eventOwner);
  });

  it("discards an older list response after event reconciliation publishes", async () => {
    const emitChunk = stubEventStream();

    const pending: Array<(value: JobsResponse) => void> = [];
    const api = {
      listJobs: vi.fn(
        () =>
          new Promise<JobsResponse>((resolve) => {
            pending.push(resolve);
          }),
      ),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(pending).toHaveLength(1));
    pending[0]?.({
      jobs: [],
      has_more: false,
      stats: { done: 0, running: 0, closed: 0, open: 0 },
    });
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));

    const olderLoad = loadJobs(store);
    await vi.waitFor(() => expect(pending).toHaveLength(2));
    emitChunk(
      '{"type":"review.completed","ts":"2026-08-04T13:00:00Z","job_id":42,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n',
    );
    await vi.waitFor(() => expect(pending).toHaveLength(3));

    pending[2]?.({
      jobs: [{ ...makeJob(42), status: "done" }],
      has_more: false,
      stats: { done: 1, running: 0, closed: 0, open: 1 },
    });
    await vi.waitFor(() => expect(store.getJobs()[0]?.status).toBe("done"));

    pending[1]?.({
      jobs: [{ ...makeJob(42), status: "running" }],
      has_more: false,
      stats: { done: 0, running: 1, closed: 0, open: 1 },
    });
    await olderLoad;

    expect(store.getJobs()[0]?.status).toBe("done");
    expect(store.getStats().running).toBe(0);
    store.disconnectEventStream(eventOwner);
  });

  it("ignores an older list failure after event reconciliation publishes", async () => {
    const emitChunk = stubEventStream();

    const pending: Array<{
      resolve: (value: JobsResponse) => void;
      reject: (reason: Error) => void;
    }> = [];
    const api = {
      listJobs: vi.fn(
        () =>
          new Promise<JobsResponse>((resolve, reject) => {
            pending.push({ resolve, reject });
          }),
      ),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(pending).toHaveLength(1));
    pending[0]?.resolve({
      jobs: [],
      has_more: false,
      stats: { done: 0, running: 0, closed: 0, open: 0 },
    });
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));

    const olderLoad = loadJobs(store);
    await vi.waitFor(() => expect(pending).toHaveLength(2));
    emitChunk(
      '{"type":"review.completed","ts":"2026-08-04T13:00:00Z","job_id":42,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n',
    );
    await vi.waitFor(() => expect(pending).toHaveLength(3));

    pending[2]?.resolve({
      jobs: [makeJob(42)],
      has_more: true,
      stats: { done: 1, running: 0, closed: 0, open: 1 },
    });
    await vi.waitFor(() => expect(store.getJobs()).toHaveLength(1));

    pending[1]?.reject(new Error("request failed"));
    await olderLoad;

    expect(store.getError()).toBeNull();
    expect(store.getHasMore()).toBe(true);
    store.disconnectEventStream(eventOwner);
  });

  it("drops an older page after event reconciliation replaces the list", async () => {
    const emitChunk = stubEventStream();

    const pending: Array<{
      resolve: (value: JobsResponse) => void;
      reject: (reason: Error) => void;
    }> = [];
    const api = {
      listJobs: vi.fn(
        () =>
          new Promise<JobsResponse>((resolve, reject) => {
            pending.push({ resolve, reject });
          }),
      ),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    const stats = { done: 11, running: 0, closed: 0, open: 11 };
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(pending).toHaveLength(1));
    pending[0]?.resolve({
      jobs: ids(10, 6).map((id) => makeJob(id)),
      has_more: true,
      next_cursor: "after-6",
      stats,
    });
    await vi.waitFor(() => expect(store.getJobs()).toHaveLength(5));

    const olderPage = loadMoreJobs(store);
    await vi.waitFor(() => expect(pending).toHaveLength(2));
    emitChunk(
      '{"type":"review.completed","ts":"2026-08-04T13:00:00Z","job_id":11,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n',
    );
    await vi.waitFor(() => expect(pending).toHaveLength(3));
    pending[2]?.resolve({
      jobs: ids(11, 7).map((id) => makeJob(id)),
      has_more: true,
      next_cursor: "after-7",
      stats,
    });
    await vi.waitFor(() =>
      expect(store.getJobs().map((job) => job.id)).toContain(11),
    );

    pending[1]?.resolve({
      jobs: ids(5, 1).map((id) => makeJob(id)),
      has_more: false,
      stats,
    });
    await olderPage;

    expect(
      store
        .getJobs()
        .map((job) => job.id)
        .sort((a, b) => b - a),
    ).toEqual(ids(11, 7));
    expect(store.getHasMore()).toBe(true);
    expect(store.areAllJobsLoaded()).toBe(false);
    expect(store.getLoadMoreError()).toBeNull();
    store.disconnectEventStream(eventOwner);
  });

  it("drops a page that started before a pending reconciliation published", async () => {
    const emitChunk = stubEventStream();

    const pending: Array<{
      resolve: (value: JobsResponse) => void;
      reject: (reason: Error) => void;
    }> = [];
    const cursors: Array<string | undefined> = [];
    const api = {
      listJobs: vi.fn(
        (query: { cursor?: string }) =>
          new Promise<JobsResponse>((resolve, reject) => {
            cursors.push(query.cursor);
            pending.push({ resolve, reject });
          }),
      ),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    const stats = { done: 11, running: 0, closed: 0, open: 11 };
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(pending).toHaveLength(1));
    pending[0]?.resolve({
      jobs: ids(10, 6).map((id) => makeJob(id)),
      has_more: true,
      next_cursor: "after-6",
      stats,
    });
    await vi.waitFor(() => expect(store.getJobs()).toHaveLength(5));

    const firstPage = loadMoreJobs(store);
    await vi.waitFor(() => expect(pending).toHaveLength(2));
    emitChunk(
      '{"type":"review.completed","ts":"2026-08-04T13:00:00Z","job_id":11,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n',
    );
    await vi.waitFor(() => expect(pending).toHaveLength(3));
    pending[1]?.resolve({
      jobs: ids(5, 1).map((id) => makeJob(id)),
      has_more: false,
      stats,
    });
    await firstPage;

    // The dropped page leaves the reload's loading state alone, so the next
    // page waits for the reload and continues from its list.
    expect(store.isLoading()).toBe(true);
    await loadMoreJobs(store);
    expect(pending).toHaveLength(3);
    pending[2]?.resolve({
      jobs: ids(11, 7).map((id) => makeJob(id)),
      has_more: true,
      next_cursor: "after-7",
      stats,
    });
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));
    const secondPage = loadMoreJobs(store);
    await vi.waitFor(() => expect(pending).toHaveLength(4));
    expect(cursors.at(-1)).toBe("after-7");
    pending[3]?.resolve({
      jobs: ids(6, 2).map((id) => makeJob(id)),
      has_more: true,
      next_cursor: "after-2",
      stats,
    });
    await secondPage;

    expect(
      store
        .getJobs()
        .map((job) => job.id)
        .sort((a, b) => b - a),
    ).toEqual(ids(11, 2));
    store.disconnectEventStream(eventOwner);
  });

  it("waits for a rerun's reload before loading the next page", async () => {
    const pending: Array<(value: JobsResponse) => void> = [];
    const cursors: Array<string | undefined> = [];
    const stats = { done: 11, running: 0, closed: 0, open: 11 };
    const api = {
      listJobs: vi.fn((query: { id?: number; cursor?: string }) => {
        if (query.id !== undefined) {
          return Promise.resolve({
            jobs: [makeJob(query.id)],
            has_more: false,
            stats,
          });
        }
        cursors.push(query.cursor);
        return new Promise<JobsResponse>((resolve) => pending.push(resolve));
      }),
      rerunJob: vi.fn().mockResolvedValue({ job_id: 8 }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    const initialLoad = loadJobs(store);
    await vi.waitFor(() => expect(pending).toHaveLength(1));
    pending[0]?.({
      jobs: ids(10, 6).map((id) => makeJob(id)),
      has_more: true,
      next_cursor: "after-6",
      stats,
    });
    await initialLoad;

    // A page appended during the rerun's reload would change the list size
    // the reload was built for, and the reload's rows would be discarded.
    store.rerunJob(8);
    await vi.waitFor(() => expect(pending).toHaveLength(2));
    expect(store.isLoading()).toBe(true);
    await loadMoreJobs(store);
    expect(pending).toHaveLength(2);

    pending[1]?.({
      jobs: [
        makeJob(11),
        makeJob(10),
        makeJob(9),
        { ...makeJob(8), status: "queued" },
        makeJob(7),
      ],
      has_more: true,
      next_cursor: "after-7",
      stats,
    });
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));
    expect(store.getJobs().find((job) => job.id === 8)?.status).toBe("queued");

    const page = loadMoreJobs(store);
    await vi.waitFor(() => expect(pending).toHaveLength(3));
    expect(cursors.at(-1)).toBe("after-7");
    pending[2]?.({
      jobs: ids(6, 2).map((id) => makeJob(id)),
      has_more: true,
      next_cursor: "after-2",
      stats,
    });
    await page;

    expect(
      store
        .getJobs()
        .map((job) => job.id)
        .sort((a, b) => b - a),
    ).toEqual(ids(11, 2));
  });

  it("invalidates the selected review for commit-scoped comment events", async () => {
    const emitChunk = stubEventStream();
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    store.setSelectedJobId(42);
    const revision = store.getSelectedReviewRevision();
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));

    emitChunk(
      '{"type":"review.commented","ts":"2026-08-04T13:00:00Z","job_id":0,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n',
    );

    await vi.waitFor(() =>
      expect(store.getSelectedReviewRevision()).toBe(revision + 1),
    );
    store.disconnectEventStream(eventOwner);
  });

  it("treats malformed event records as a reconnectable stream failure", async () => {
    vi.useFakeTimers();
    const encoder = new TextEncoder();
    const bodies: ReadableStreamDefaultController<Uint8Array>[] = [];
    globalThis.fetch = vi.fn(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              bodies.push(controller);
            },
          }),
          { status: 200 },
        ),
    );
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(bodies).toHaveLength(1));
    bodies[0]?.enqueue(encoder.encode("not-json\n"));
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(false));
    await vi.advanceTimersByTimeAsync(500);

    await vi.waitFor(() => expect(bodies).toHaveLength(2));
    expect(api.listJobs).toHaveBeenCalled();
    store.disconnectEventStream(eventOwner);
  });

  it("cancels an open response body when the stream returns an error status", async () => {
    let bodyCancelled = false;
    let signal: AbortSignal | undefined;
    const body = new ReadableStream<Uint8Array>({
      cancel() {
        bodyCancelled = true;
      },
    });
    globalThis.fetch = vi.fn(
      async (_input: RequestInfo | URL, init?: RequestInit) => {
        signal = fetchSignal(_input, init);
        return new Response(body, { status: 503 });
      },
    );
    const store = createJobsStore({ api: {} as never, navigate: vi.fn() });

    const eventOwner = store.connectEventStream("/api/roborev");

    try {
      await vi.waitFor(() => expect(bodyCancelled).toBe(true));
    } finally {
      store.disconnectEventStream(eventOwner);
    }
    expect(signal?.aborted).toBe(true);
  });

  it("reconnects with backoff and stops retrying after disconnect", async () => {
    vi.useFakeTimers();
    const bodies: ReadableStreamDefaultController<Uint8Array>[] = [];
    globalThis.fetch = vi.fn(async () => {
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          bodies.push(controller);
        },
      });
      return new Response(body, { status: 200 });
    });
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(bodies).toHaveLength(1));
    bodies[0]?.close();
    await vi.advanceTimersByTimeAsync(499);
    expect(globalThis.fetch).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(1);
    await vi.waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2));
    store.disconnectEventStream(eventOwner);
    await vi.advanceTimersByTimeAsync(60_000);

    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
  });

  it("opens the replacement event stream before reconciling its checkpoint", async () => {
    vi.useFakeTimers();
    const sequence: string[] = [];
    const bodies: ReadableStreamDefaultController<Uint8Array>[] = [];
    globalThis.fetch = vi.fn(async () => {
      sequence.push(`stream:${bodies.length + 1}`);
      return new Response(
        new ReadableStream<Uint8Array>({
          start(controller) {
            bodies.push(controller);
          },
        }),
        { status: 200 },
      );
    });
    const api = {
      listJobs: vi.fn().mockImplementation(async () => {
        sequence.push("jobs");
        return {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        };
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(bodies).toHaveLength(1));
    bodies[0]?.close();
    await vi.advanceTimersByTimeAsync(1_000);
    await vi.waitFor(() => expect(bodies).toHaveLength(2));

    const secondStream = sequence.indexOf("stream:2");
    expect(secondStream).toBeGreaterThan(0);
    expect(sequence.indexOf("jobs", secondStream)).toBeGreaterThan(
      secondStream,
    );
    store.disconnectEventStream(eventOwner);
  });

  it("keeps a reopened event stream disconnected until reconciliation succeeds", async () => {
    vi.useFakeTimers();
    const bodies: ReadableStreamDefaultController<Uint8Array>[] = [];
    globalThis.fetch = vi.fn(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              bodies.push(controller);
            },
          }),
          { status: 200 },
        ),
    );
    let reconciliationRound = 0;
    const api = {
      listJobs: vi.fn().mockImplementation(async () => {
        reconciliationRound += 1;
        if (reconciliationRound <= 2) {
          throw new TypeError("authority unavailable");
        }
        return {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        };
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(bodies).toHaveLength(1));
    bodies[0]?.close();
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(false));
    await vi.advanceTimersByTimeAsync(500);
    await vi.waitFor(() => expect(bodies).toHaveLength(2));

    expect(store.isEventStreamConnected()).toBe(false);
    await vi.advanceTimersByTimeAsync(500);
    await vi.waitFor(() => expect(bodies).toHaveLength(3));
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));
    store.disconnectEventStream(eventOwner);
  });
});

describe("createJobsStore auto-design filter", () => {
  function makeApi() {
    return {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
  }

  it("sends hide_classify_jobs by default and drops it when showAutoDesign is on", async () => {
    const api = makeApi();
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);

    expect(store.getFilterShowAutoDesign()).toBe(false);
    expect(api.listJobs).toHaveBeenLastCalledWith(
      expect.objectContaining({
        hide_classify_jobs: "true",
      }),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );

    store.setFilter("showAutoDesign", true);
    await vi.waitFor(() => {
      expect(api.listJobs.mock.calls.length).toBeGreaterThan(1);
    });

    const lastQuery = api.listJobs.mock.calls.at(-1)?.[0] as Record<
      string,
      unknown
    >;
    expect(lastQuery).not.toHaveProperty("hide_classify_jobs");
  });
});

describe("createJobsStore filtered status counts", () => {
  it("uses server-side aggregate counts without downloading an unbounded job list", async () => {
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [makeJob(1)],
        has_more: true,
        stats: {
          queued: 1,
          running: 2,
          done: 30,
          failed: 4,
          canceled: 5,
          skipped: 6,
          closed: 20,
          open: 10,
        },
        filtered_stats: {
          queued: 1,
          running: 2,
          done: 12,
          failed: 3,
          canceled: 2,
          skipped: 1,
          closed: 0,
          open: 9,
        },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);

    expect(store.getFilteredStatusCounts()).toEqual({
      queued: 1,
      running: 2,
      done: 12,
      failed: 3,
    });
    expect(api.listJobs).toHaveBeenCalledTimes(1);
    expect(api.listJobs).toHaveBeenCalledWith(
      expect.objectContaining({
        hide_classify_jobs: "true",
        limit: 50,
        omit_prompt: "true",
      }),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    const query = api.listJobs.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(query.limit).not.toBe(0);
  });
});

describe("createJobsStore pagination", () => {
  function paginatedApi() {
    const firstPage = Array.from({ length: 50 }, (_, index) =>
      makeJob(100 - index),
    );
    const secondPage = Array.from({ length: 50 }, (_, index) =>
      makeJob(50 - index),
    );
    const refreshedPage = Array.from({ length: 100 }, (_, index) =>
      makeJob(101 - index),
    );
    return {
      listJobs: vi.fn().mockImplementation(async (query) => {
        const typedQuery = query as {
          cursor?: string;
          limit?: number;
          status?: string;
        };
        if (query.status) {
          return {
            jobs: [],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          };
        }
        if (typedQuery.cursor !== undefined) {
          const second = typedQuery.cursor === "after-51";
          return {
            jobs: second ? secondPage : [],
            has_more: second,
            next_cursor: second ? "after-1" : null,
            stats: { done: 100, closed: 0, open: 100 },
          };
        }
        const refreshed = typedQuery.limit === 100;
        return {
          jobs: refreshed ? refreshedPage : firstPage,
          has_more: true,
          next_cursor: refreshed ? "after-2" : "after-51",
          stats: { done: 101, closed: 0, open: 101 },
        };
      }),
    };
  }

  it("preserves the loaded depth during an authoritative refresh", async () => {
    const api = paginatedApi();
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    expect(store.getJobs()).toHaveLength(100);

    await loadJobs(store);

    expect(store.getJobs()).toHaveLength(100);
    expect(api.listJobs.mock.calls.at(-1)?.[0].limit).toBe(100);
  });

  it("resets the loaded depth when filters change", async () => {
    const api = paginatedApi();
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    store.setFilter("status", "failed");
    await vi.waitFor(() =>
      expect(api.listJobs.mock.calls.at(-1)?.[0].status).toBe("failed"),
    );

    expect(api.listJobs.mock.calls.at(-1)?.[0].limit).toBe(50);
  });

  it("keeps subsequent pagination requests at one page", async () => {
    const api = paginatedApi();
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    await loadMoreJobs(store);

    const paginationCalls = api.listJobs.mock.calls.filter(
      (call) => call[0].cursor !== undefined,
    );
    expect(paginationCalls).toHaveLength(2);
    expect(paginationCalls[1]?.[0].limit).toBe(50);
  });

  it("uses the opaque cursor returned with the current page", async () => {
    const recentlyRerun = {
      ...makeJob(1),
      enqueued_at: "2026-04-11T12:00:00Z",
    };
    const olderHighId = {
      ...makeJob(100),
      enqueued_at: "2026-04-11T10:00:00Z",
    };
    const api = {
      listJobs: vi.fn().mockImplementation(async (query) => {
        const typedQuery = query as { cursor?: string };
        return {
          jobs:
            typedQuery.cursor === undefined ? [recentlyRerun, olderHighId] : [],
          has_more: typedQuery.cursor === undefined,
          next_cursor:
            typedQuery.cursor === undefined ? "stable-enqueue-cursor" : null,
          stats: { done: 2, closed: 0, open: 2 },
        };
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);

    expect(store.getJobs().map((job) => job.id)).toEqual([1, 100]);
    expect(store.getSortColumn()).toBe("enqueued_at");
    expect(store.areAllJobsLoaded()).toBe(false);

    await loadMoreJobs(store);

    expect(api.listJobs.mock.calls[1]?.[0].cursor).toBe(
      "stable-enqueue-cursor",
    );
  });

  it("does not load beyond the daemon result limit", async () => {
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: Array.from({ length: 10_000 }, (_, index) =>
          makeJob(10_000 - index),
        ),
        has_more: true,
        stats: { done: 10_001, closed: 0, open: 10_001 },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);

    expect(api.listJobs).toHaveBeenCalledTimes(1);
    expect(store.getHasMore()).toBe(false);
  });

  it("preserves loaded depth when an unchanged filter is reapplied", async () => {
    const api = paginatedApi();
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    store.setFilter("status", undefined);

    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalledTimes(3));
    expect(api.listJobs.mock.calls.at(-1)?.[0].limit).toBe(100);
  });

  it("preserves loaded depth when the same repo and branch are reapplied", async () => {
    const api = paginatedApi();
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    store.setRepoBranchFilter(undefined, undefined);

    await vi.waitFor(() => expect(api.listJobs).toHaveBeenCalledTimes(3));
    expect(api.listJobs.mock.calls.at(-1)?.[0].limit).toBe(100);
  });
});

describe("createJobsStore panel expansion", () => {
  function deferred<T>() {
    let resolve!: (value: T) => void;
    const promise = new Promise<T>((res) => {
      resolve = res;
    });
    return { promise, resolve };
  }

  function makePanelParent(id: number): ReviewJob {
    return {
      ...makeJob(id),
      job_type: "synthesis",
      panel_role: "synthesis",
      panel_run_uuid: `run-${id}`,
      panel_summary: {
        panel_run_uuid: `run-${id}`,
        members_total: 2,
        members_terminal: 2,
        members_succeeded: 2,
        members_failed: 0,
        members_canceled: 0,
        members_skipped: 0,
      },
    };
  }

  function makeMember(id: number, runUuid: string, index: number): ReviewJob {
    return {
      ...makeJob(id),
      panel_role: "member",
      panel_run_uuid: runUuid,
      panel_member_index: index,
      panel_member_name: index === 0 ? "default" : "security",
    };
  }

  it("lazily fetches members sorted by panel_member_index on first expand", async () => {
    const parent = makePanelParent(10);
    const members = [makeMember(12, "run-10", 1), makeMember(11, "run-10", 0)];
    const api = {
      listJobs: vi.fn().mockImplementation((query: Record<string, unknown>) => {
        if (query.panel_run === "run-10") {
          return Promise.resolve({
            jobs: [parent, ...members],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          });
        }
        return Promise.resolve({
          jobs: [parent],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        });
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);

    expect(store.isPanelExpanded("run-10")).toBe(false);
    store.togglePanel(parent);
    expect(store.isPanelExpanded("run-10")).toBe(true);
    await vi.waitFor(() => {
      expect(store.getPanelMembers("run-10")).toBeDefined();
    });
    expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([11, 12]);
    expect(api.listJobs).toHaveBeenCalledWith(
      { panel_run: "run-10", limit: 0, omit_prompt: "true" },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );

    const calls = api.listJobs.mock.calls.length;
    store.togglePanel(parent);
    store.togglePanel(parent);
    expect(store.isPanelExpanded("run-10")).toBe(true);
    expect(api.listJobs.mock.calls.length).toBe(calls);
  });

  it("refreshes members of expanded panels when the listing reloads", async () => {
    const parent = makePanelParent(10);
    const api = {
      listJobs: vi.fn().mockResolvedValue({
        jobs: [parent],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);
    store.togglePanel(parent);
    await vi.waitFor(() =>
      expect(store.getPanelMembers("run-10")).toBeDefined(),
    );

    const before = api.listJobs.mock.calls.filter(
      (c) => c[0].panel_run === "run-10",
    ).length;
    await loadJobs(store);
    await vi.waitFor(() => {
      const after = api.listJobs.mock.calls.filter(
        (c) => c[0].panel_run === "run-10",
      ).length;
      expect(after).toBe(before + 1);
    });
  });

  it("includes expanded panel members in highlight navigation", async () => {
    const parent = makePanelParent(10);
    const members = [makeMember(12, "run-10", 1), makeMember(11, "run-10", 0)];
    const api = {
      listJobs: vi.fn().mockImplementation((query: Record<string, unknown>) => {
        if (query.panel_run === "run-10") {
          return Promise.resolve({
            jobs: [parent, ...members],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          });
        }
        return Promise.resolve({
          jobs: [parent],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        });
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);
    store.togglePanel(parent);
    await vi.waitFor(() => {
      expect(store.getVisibleJobs().map((j) => j.id)).toEqual([10, 11, 12]);
    });

    store.highlightJob(10);
    store.highlightNextJob();
    expect(store.getHighlightedJobId()).toBe(11);
    store.highlightNextJob();
    expect(store.getHighlightedJobId()).toBe(12);
    store.highlightPrevJob();
    expect(store.getHighlightedJobId()).toBe(11);
    await loadJobs(store);
    expect(store.getHighlightedJobId()).toBe(11);
  });

  it("keeps cached members visible in navigation while a refresh is loading", async () => {
    const parent = makePanelParent(10);
    const slowRefresh = deferred<{
      jobs: ReviewJob[];
      has_more: boolean;
      stats: { done: number; closed: number; open: number };
    }>();
    let panelCalls = 0;
    const api = {
      listJobs: vi.fn().mockImplementation((query: Record<string, unknown>) => {
        if (query.panel_run === "run-10") {
          panelCalls++;
          if (panelCalls === 1) {
            return Promise.resolve({
              jobs: [parent, makeMember(11, "run-10", 0)],
              has_more: false,
              stats: { done: 0, closed: 0, open: 0 },
            });
          }
          return slowRefresh.promise;
        }
        return Promise.resolve({
          jobs: [parent],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        });
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);
    store.togglePanel(parent);
    await vi.waitFor(() => {
      expect(store.getVisibleJobs().map((j) => j.id)).toEqual([10, 11]);
    });

    store.highlightJob(11);
    await loadJobs(store);
    await vi.waitFor(() => expect(panelCalls).toBe(2));

    expect(store.getVisibleJobs().map((j) => j.id)).toEqual([10, 11]);
    expect(store.getHighlightedJobId()).toBe(11);

    slowRefresh.resolve({
      jobs: [parent, makeMember(12, "run-10", 0)],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });
    await vi.waitFor(() => {
      expect(store.getVisibleJobs().map((j) => j.id)).toEqual([10, 12]);
    });
    expect(store.getHighlightedJobId()).toBe(10);
  });

  it("moves highlight to the parent when closing a panel from a member row", async () => {
    const parent = makePanelParent(10);
    const api = {
      listJobs: vi.fn().mockImplementation((query: Record<string, unknown>) => {
        if (query.panel_run === "run-10") {
          return Promise.resolve({
            jobs: [parent, makeMember(11, "run-10", 0)],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          });
        }
        return Promise.resolve({
          jobs: [parent],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        });
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);
    store.togglePanel(parent);
    await vi.waitFor(() => {
      expect(store.getVisibleJobs().map((j) => j.id)).toEqual([10, 11]);
    });

    store.highlightJob(11);
    store.togglePanel(parent);

    expect(store.isPanelExpanded("run-10")).toBe(false);
    expect(store.getVisibleJobs().map((j) => j.id)).toEqual([10]);
    expect(store.getHighlightedJobId()).toBe(10);
  });

  it("refreshes interested panel members on listing reload while the table panel is collapsed", async () => {
    const parent = makePanelParent(10);
    let panelCalls = 0;
    const api = {
      listJobs: vi.fn().mockImplementation((query: Record<string, unknown>) => {
        if (query.panel_run === "run-10") {
          panelCalls++;
          return Promise.resolve({
            jobs: [parent, makeMember(panelCalls === 1 ? 11 : 12, "run-10", 0)],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          });
        }
        return Promise.resolve({
          jobs: [parent],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        });
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);

    store.setPanelMemberInterest("run-10");
    await vi.waitFor(() => {
      expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([11]);
    });

    await loadJobs(store);
    await vi.waitFor(() => {
      expect(panelCalls).toBe(2);
      expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([12]);
    });
  });

  it("drains queued interested refreshes while the table panel is collapsed", async () => {
    const parent = makePanelParent(10);
    const initialFetch = deferred<{
      jobs: ReviewJob[];
      has_more: boolean;
      stats: { done: number; closed: number; open: number };
    }>();
    let panelCalls = 0;
    const api = {
      listJobs: vi.fn().mockImplementation((query: Record<string, unknown>) => {
        if (query.panel_run === "run-10") {
          panelCalls++;
          if (panelCalls === 1) return initialFetch.promise;
          return Promise.resolve({
            jobs: [parent, makeMember(12, "run-10", 0)],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          });
        }
        return Promise.resolve({
          jobs: [parent],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        });
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);

    store.setPanelMemberInterest("run-10");
    await vi.waitFor(() => expect(panelCalls).toBe(1));
    await loadJobs(store);
    expect(panelCalls).toBe(1);

    initialFetch.resolve({
      jobs: [parent, makeMember(11, "run-10", 0)],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });

    await vi.waitFor(() => {
      expect(panelCalls).toBe(2);
      expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([12]);
    });
  });

  it("coalesces panel refreshes while a member request is in flight", async () => {
    const parent = makePanelParent(10);
    const duplicateVisibleMember = makeMember(99, "run-10", 1);
    const slowRefresh = deferred<{
      jobs: ReviewJob[];
      has_more: boolean;
      stats: { done: number; closed: number; open: number };
    }>();
    let panelCalls = 0;
    const api = {
      listJobs: vi.fn().mockImplementation((query: Record<string, unknown>) => {
        if (query.panel_run === "run-10") {
          panelCalls++;
          if (panelCalls === 1) {
            return Promise.resolve({
              jobs: [parent, makeMember(11, "run-10", 0)],
              has_more: false,
              stats: { done: 0, closed: 0, open: 0 },
            });
          }
          if (panelCalls === 2) return slowRefresh.promise;
          return Promise.resolve({
            jobs: [parent, makeMember(13, "run-10", 0)],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          });
        }
        return Promise.resolve({
          jobs: [parent, duplicateVisibleMember],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        });
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);
    store.togglePanel(parent);
    await vi.waitFor(() => {
      expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([11]);
    });

    await loadJobs(store);
    await vi.waitFor(() => expect(panelCalls).toBe(2));
    await loadJobs(store);
    expect(panelCalls).toBe(2);

    slowRefresh.resolve({
      jobs: [parent, makeMember(12, "run-10", 0)],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });

    await vi.waitFor(() => {
      expect(panelCalls).toBe(3);
      expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([13]);
    });
  });

  it("keeps accepted members when a stale in-flight refresh is followed by a failed latest refresh", async () => {
    const parent = makePanelParent(10);
    const staleRefresh = deferred<{
      jobs: ReviewJob[];
      has_more: boolean;
      stats: { done: number; closed: number; open: number };
    }>();
    const onError = vi.fn();
    let panelCalls = 0;
    const api = {
      listJobs: vi.fn().mockImplementation((query: Record<string, unknown>) => {
        if (query.panel_run === "run-10") {
          panelCalls++;
          if (panelCalls === 1) {
            return Promise.resolve({
              jobs: [parent, makeMember(11, "run-10", 0)],
              has_more: false,
              stats: { done: 0, closed: 0, open: 0 },
            });
          }
          if (panelCalls === 2) return staleRefresh.promise;
          return Promise.reject(new Response(null, { status: 503 }));
        }
        return Promise.resolve({
          jobs: [parent],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        });
      }),
    };
    const store = createJobsStore({
      api: api as never,
      navigate: vi.fn(),
      onError,
    });
    await loadJobs(store);
    store.togglePanel(parent);
    await vi.waitFor(() => {
      expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([11]);
    });

    await loadJobs(store);
    await vi.waitFor(() => expect(panelCalls).toBe(2));
    await loadJobs(store);
    expect(panelCalls).toBe(2);

    staleRefresh.resolve({
      jobs: [parent, makeMember(12, "run-10", 0)],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });

    await vi.waitFor(() => {
      expect(panelCalls).toBe(3);
      expect(onError).toHaveBeenCalledWith("Failed to load panel members");
    });
    expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([11]);
    expect(store.getPanelMemberError("run-10")).toBe(
      "Failed to load panel members",
    );
  });
});
