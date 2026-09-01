import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { OwnedAppRuntime } from "../../runtime/runtime";
import { makeTestAppRuntime } from "../../testing/runtime";
import {
  createJobsStore as createRuntimeJobsStore,
  type JobsStore,
  type JobsStoreOptions,
} from "./jobs.svelte";
import type { components } from "../../api/generated";

type ReviewJob = components["schemas"]["ReviewJob"];

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

describe("createJobsStore filter preferences", () => {
  it("maps the branchless display sentinel to the empty branch query", async () => {
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
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
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    store.setFilter("showAutoDesign", true);
    store.setFilter("branch", "(none)");
    await vi.waitFor(() => expect(client.GET).toHaveBeenCalled());

    expect(client.GET.mock.calls.at(-1)?.[1]?.params.query).toEqual(
      expect.objectContaining({ branch_empty: "true", limit: 50 }),
    );
    expect(client.GET.mock.calls.at(-1)?.[1]?.params.query).not.toHaveProperty(
      "branch",
    );
    expect(store.usesFilteredStatusCounts()).toBe(true);
    expect(store.getFilteredStatusCounts()).toEqual({
      queued: 1,
      running: 0,
      done: 0,
      failed: 0,
    });
  });

  it("restores filter choices in a new store and applies them to the jobs query", async () => {
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const firstStore = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    firstStore.setFilter("hideClosed", true);
    firstStore.setFilter("showAutoDesign", true);
    firstStore.dispose();

    const restoredStore = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });
    await loadJobs(restoredStore);

    expect(restoredStore.getFilterHideClosed()).toBe(true);
    expect(restoredStore.getFilterShowAutoDesign()).toBe(true);
    expect(client.GET).toHaveBeenCalledWith(
      "/api/jobs",
      expect.objectContaining({
        params: {
          query: { closed: "false", limit: 50, omit_prompt: "true" },
        },
      }),
    );
  });

  it("synchronizes filter changes across live jobs stores", async () => {
    const firstClient = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const secondClient = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const firstStore = createJobsStore({
      client: firstClient as never,
      navigate: vi.fn(),
    });
    const secondStore = createJobsStore({
      client: secondClient as never,
      navigate: vi.fn(),
    });

    firstStore.setFilter("hideClosed", true);
    firstStore.setFilter("showAutoDesign", true);

    expect(secondStore.getFilterHideClosed()).toBe(true);
    expect(secondStore.getFilterShowAutoDesign()).toBe(true);
    await vi.waitFor(() =>
      expect(secondClient.GET).toHaveBeenCalledWith(
        "/api/jobs",
        expect.objectContaining({
          params: {
            query: { closed: "false", limit: 50, omit_prompt: "true" },
          },
        }),
      ),
    );
  });

  it("keeps filtering when browser storage is unavailable", async () => {
    const getItem = vi
      .spyOn(Storage.prototype, "getItem")
      .mockImplementation(() => {
        throw new Error("storage unavailable");
      });
    const firstClient = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const secondClient = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const setItem = vi
      .spyOn(Storage.prototype, "setItem")
      .mockImplementation(() => {
        throw new Error("storage unavailable");
      });

    try {
      const firstStore = createJobsStore({
        client: firstClient as never,
        navigate: vi.fn(),
      });
      expect(firstStore.getFilterHideClosed()).toBe(false);
      expect(firstStore.getFilterShowAutoDesign()).toBe(false);

      firstStore.setFilter("hideClosed", true);
      firstStore.setFilter("showAutoDesign", true);
      const secondStore = createJobsStore({
        client: secondClient as never,
        navigate: vi.fn(),
      });
      await loadJobs(secondStore);

      expect(secondStore.getFilterHideClosed()).toBe(true);
      expect(secondStore.getFilterShowAutoDesign()).toBe(true);
      expect(secondClient.GET).toHaveBeenCalledWith(
        "/api/jobs",
        expect.objectContaining({
          params: {
            query: { closed: "false", limit: 50, omit_prompt: "true" },
          },
        }),
      );
    } finally {
      getItem.mockRestore();
      setItem.mockRestore();
    }
  });
});

describe("createJobsStore cost sorting", () => {
  function makeCostJob(id: number, tokenUsage?: string): ReviewJob {
    return {
      ...makeJob(id),
      ...(tokenUsage !== undefined ? { token_usage: tokenUsage } : {}),
    };
  }

  it("sorts missing cost before zero-dollar jobs", async () => {
    const jobs: ReviewJob[] = [
      makeCostJob(8),
      makeCostJob(2, JSON.stringify({ has_cost: true, cost_usd: 0.5 })),
      makeCostJob(6, JSON.stringify({ has_cost: true, cost_usd: 0 })),
      makeCostJob(5, "not json"),
    ];
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs,
          has_more: false,
          stats: { done: 1, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };

    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    store.setSortColumn("cost");

    expect(store.getSortColumn()).toBe("cost");
    expect(store.getSortDirection()).toBe("asc");
    expect(store.getJobs().map((job) => job.id)).toEqual([8, 5, 6, 2]);

    store.setSortColumn("cost");

    expect(store.getSortDirection()).toBe("desc");
    expect(store.getJobs().map((job) => job.id)).toEqual([2, 6, 8, 5]);
  });
});

describe("createJobsStore review type sorting", () => {
  it("sorts synthesis jobs as panels", async () => {
    const jobs: ReviewJob[] = [
      { ...makeJob(1), review_type: "security" },
      { ...makeJob(2), job_type: "synthesis", panel_role: "synthesis" },
      makeJob(3),
    ];
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs,
          has_more: false,
          stats: { done: 3, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    store.setSortColumn("review_type");

    expect(store.getJobs().map((job) => job.id)).toEqual([3, 2, 1]);
  });
});

describe("createJobsStore elapsed sorting", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-04-11T12:00:00Z"));
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it("sorts missing elapsed before zero-second durations", async () => {
    const jobs: ReviewJob[] = [
      makeJob(8, "2026-04-11T11:45:00Z"),
      makeJob(2, "2026-04-11T11:00:00Z", "2026-04-11T11:05:00Z"),
      makeJob(6, "2026-04-11T11:30:00Z", "2026-04-11T11:30:00Z"),
      makeJob(5),
    ];
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs,
          has_more: false,
          stats: { done: 1, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };

    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    store.setSortColumn("elapsed");

    expect(store.getSortColumn()).toBe("elapsed");
    expect(store.getSortDirection()).toBe("asc");
    expect(store.getJobs().map((job) => job.id)).toEqual([5, 6, 2, 8]);

    store.setSortColumn("elapsed");

    expect(store.getSortDirection()).toBe("desc");
    expect(store.getJobs().map((job) => job.id)).toEqual([8, 2, 6, 5]);
  });
});

describe("createJobsStore event stream", () => {
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
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
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
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
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
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));
    expect(client.GET).toHaveBeenCalledTimes(1);
    bodyController?.enqueue(encoder.encode('{"type":"review.com'));
    bodyController?.enqueue(
      encoder.encode(
        'pleted","ts":"2026-08-04T13:00:00Z","job_id":42,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n',
      ),
    );

    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(2));
    store.disconnectEventStream(eventOwner);

    await vi.waitFor(() => expect(bodyCancelled).toBe(true));
    await vi.waitFor(() => expect(signal?.aborted).toBe(true));
    expect(store.isEventStreamConnected()).toBe(false);
  });

  it("keeps event reconciliation alive across overlapping list requests", async () => {
    const encoder = new TextEncoder();
    let bodyController: ReadableStreamDefaultController<Uint8Array> | undefined;
    globalThis.fetch = vi.fn(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              bodyController = controller;
            },
          }),
          { status: 200 },
        ),
    );
    let requestCount = 0;
    let firstSignal: AbortSignal | undefined;
    let resolveFirst:
      | ((value: {
          data: {
            jobs: never[];
            has_more: boolean;
            stats: { done: number; closed: number; open: number };
          };
          error: undefined;
        }) => void)
      | undefined;
    const client = {
      GET: vi.fn(
        (_path: string, options: { signal?: AbortSignal } | undefined) => {
          requestCount += 1;
          if (requestCount !== 2) {
            return Promise.resolve({
              data: {
                jobs: [],
                has_more: false,
                stats: { done: 0, closed: 0, open: 0 },
              },
              error: undefined,
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
      client: client as never,
      navigate: vi.fn(),
    });
    store.setSelectedJobId(42);
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));
    expect(client.GET).toHaveBeenCalledTimes(1);

    bodyController?.enqueue(
      encoder.encode(
        '{"type":"review.completed","ts":"2026-08-04T13:00:00Z","job_id":42}\n',
      ),
    );
    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(2));
    await loadJobs(store);
    expect(client.GET).toHaveBeenCalledTimes(3);
    expect(firstSignal?.aborted).toBe(false);
    resolveFirst?.({
      data: {
        jobs: [],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      },
      error: undefined,
    });

    bodyController?.enqueue(
      encoder.encode(
        '{"type":"review.closed","ts":"2026-08-04T13:01:00Z","job_id":42}\n',
      ),
    );
    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(4));

    expect(store.isEventStreamConnected()).toBe(true);
    store.disconnectEventStream(eventOwner);
  });

  it("drains burst events while coalescing a slow reconciliation", async () => {
    const encoder = new TextEncoder();
    let bodyController: ReadableStreamDefaultController<Uint8Array> | undefined;
    globalThis.fetch = vi.fn(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              bodyController = controller;
            },
          }),
          { status: 200 },
        ),
    );

    type JobsResponse = {
      data: {
        jobs: ReviewJob[];
        has_more: boolean;
        stats: { done: number; running: number; closed: number; open: number };
      };
      error: undefined;
    };
    const pending: Array<(value: JobsResponse) => void> = [];
    let requestCount = 0;
    const client = {
      GET: vi.fn(() => {
        requestCount += 1;
        if (requestCount === 1) {
          return Promise.resolve({
            data: {
              jobs: [],
              has_more: false,
              stats: { done: 0, running: 0, closed: 0, open: 0 },
            },
            error: undefined,
          });
        }
        return new Promise<JobsResponse>((resolve) => pending.push(resolve));
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });
    store.setSelectedJobId(42);
    const initialRevision = store.getSelectedReviewRevision();
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));

    const emitEvent = (index: number) => {
      bodyController?.enqueue(
        encoder.encode(
          `{"type":"review.completed","ts":"2026-08-04T13:00:${String(index).padStart(2, "0")}Z","job_id":42,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n`,
        ),
      );
    };
    emitEvent(0);
    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(2));
    for (let index = 1; index < 12; index += 1) {
      emitEvent(index);
    }

    await vi.waitFor(() =>
      expect(store.getSelectedReviewRevision()).toBe(initialRevision + 12),
    );
    pending[0]?.({
      data: {
        jobs: [{ ...makeJob(42), status: "running" }],
        has_more: false,
        stats: { done: 0, running: 1, closed: 0, open: 1 },
      },
      error: undefined,
    });
    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(3));
    pending[1]?.({
      data: {
        jobs: [{ ...makeJob(42), status: "done" }],
        has_more: false,
        stats: { done: 1, running: 0, closed: 0, open: 1 },
      },
      error: undefined,
    });
    await vi.waitFor(() => expect(store.getJobs()[0]?.status).toBe("done"));
    store.disconnectEventStream(eventOwner);
  });

  it("discards an older list response after event reconciliation publishes", async () => {
    const encoder = new TextEncoder();
    let bodyController: ReadableStreamDefaultController<Uint8Array> | undefined;
    globalThis.fetch = vi.fn(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              bodyController = controller;
            },
          }),
          { status: 200 },
        ),
    );

    type JobsResponse = {
      data: {
        jobs: ReviewJob[];
        has_more: boolean;
        stats: { done: number; running: number; closed: number; open: number };
      };
      error: undefined;
    };
    const pending: Array<(value: JobsResponse) => void> = [];
    const client = {
      GET: vi.fn(
        () =>
          new Promise<JobsResponse>((resolve) => {
            pending.push(resolve);
          }),
      ),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(pending).toHaveLength(1));
    pending[0]?.({
      data: {
        jobs: [],
        has_more: false,
        stats: { done: 0, running: 0, closed: 0, open: 0 },
      },
      error: undefined,
    });
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));

    const olderLoad = loadJobs(store);
    await vi.waitFor(() => expect(pending).toHaveLength(2));
    bodyController?.enqueue(
      encoder.encode(
        '{"type":"review.completed","ts":"2026-08-04T13:00:00Z","job_id":42,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n',
      ),
    );
    await vi.waitFor(() => expect(pending).toHaveLength(3));

    pending[2]?.({
      data: {
        jobs: [{ ...makeJob(42), status: "done" }],
        has_more: false,
        stats: { done: 1, running: 0, closed: 0, open: 1 },
      },
      error: undefined,
    });
    await vi.waitFor(() => expect(store.getJobs()[0]?.status).toBe("done"));

    pending[1]?.({
      data: {
        jobs: [{ ...makeJob(42), status: "running" }],
        has_more: false,
        stats: { done: 0, running: 1, closed: 0, open: 1 },
      },
      error: undefined,
    });
    await olderLoad;

    expect(store.getJobs()[0]?.status).toBe("done");
    expect(store.getStats().running).toBe(0);
    store.disconnectEventStream(eventOwner);
  });

  it("invalidates the selected review for commit-scoped comment events", async () => {
    const encoder = new TextEncoder();
    let bodyController: ReadableStreamDefaultController<Uint8Array> | undefined;
    globalThis.fetch = vi.fn(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              bodyController = controller;
            },
          }),
          { status: 200 },
        ),
    );
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });
    store.setSelectedJobId(42);
    const revision = store.getSelectedReviewRevision();
    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(true));

    bodyController?.enqueue(
      encoder.encode(
        '{"type":"review.commented","ts":"2026-08-04T13:00:00Z","job_id":0,"repo":"/workspace/repo","repo_name":"repo","sha":"abc123"}\n',
      ),
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
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    const eventOwner = store.connectEventStream("/api/roborev");
    await vi.waitFor(() => expect(bodies).toHaveLength(1));
    bodies[0]?.enqueue(encoder.encode("not-json\n"));
    await vi.waitFor(() => expect(store.isEventStreamConnected()).toBe(false));
    await vi.advanceTimersByTimeAsync(500);

    await vi.waitFor(() => expect(bodies).toHaveLength(2));
    expect(client.GET).toHaveBeenCalled();
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
    const store = createJobsStore({ client: {} as never, navigate: vi.fn() });

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
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
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
    const client = {
      GET: vi.fn().mockImplementation(async () => {
        sequence.push("jobs");
        return {
          data: {
            jobs: [],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          },
          error: undefined,
        };
      }),
    };
    const store = createJobsStore({
      client: client as never,
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
    const client = {
      GET: vi.fn().mockImplementation(async () => {
        reconciliationRound += 1;
        if (reconciliationRound <= 2) {
          return {
            data: undefined,
            error: { message: "authority unavailable" },
          };
        }
        return {
          data: {
            jobs: [],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          },
          error: undefined,
        };
      }),
    };
    const store = createJobsStore({
      client: client as never,
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
  function makeClient() {
    return {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
  }

  it("sends hide_classify_jobs by default and drops it when showAutoDesign is on", async () => {
    const client = makeClient();
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);

    expect(store.getFilterShowAutoDesign()).toBe(false);
    expect(client.GET).toHaveBeenLastCalledWith(
      "/api/jobs",
      expect.objectContaining({
        params: {
          query: expect.objectContaining({ hide_classify_jobs: "true" }),
        },
      }),
    );

    store.setFilter("showAutoDesign", true);
    await vi.waitFor(() => {
      expect(client.GET.mock.calls.length).toBeGreaterThan(1);
    });

    const lastQuery = client.GET.mock.calls.at(-1)?.[1]?.params
      ?.query as Record<string, unknown>;
    expect(lastQuery).not.toHaveProperty("hide_classify_jobs");
  });
});

describe("createJobsStore filtered status counts", () => {
  it("uses server-side aggregate counts without downloading an unbounded job list", async () => {
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
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
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);

    expect(store.getFilteredStatusCounts()).toEqual({
      queued: 1,
      running: 2,
      done: 12,
      failed: 3,
    });
    expect(client.GET).toHaveBeenCalledTimes(1);
    expect(client.GET).toHaveBeenCalledWith(
      "/api/jobs",
      expect.objectContaining({
        params: {
          query: expect.objectContaining({
            hide_classify_jobs: "true",
            limit: 50,
            omit_prompt: "true",
          }),
        },
      }),
    );
    const query = client.GET.mock.calls[0]?.[1]?.params?.query as Record<
      string,
      unknown
    >;
    expect(query.limit).not.toBe(0);
  });
});

describe("createJobsStore pagination", () => {
  function paginatedClient() {
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
      GET: vi.fn().mockImplementation(async (_path, options) => {
        const query = options.params.query as {
          before?: number;
          limit?: number;
          status?: string;
        };
        if (query.status) {
          return {
            data: {
              jobs: [],
              has_more: false,
              stats: { done: 0, closed: 0, open: 0 },
            },
            error: undefined,
          };
        }
        if (query.before !== undefined) {
          return {
            data: {
              jobs: query.before === 51 ? secondPage : [],
              has_more: query.before === 51,
              stats: { done: 100, closed: 0, open: 100 },
            },
            error: undefined,
          };
        }
        return {
          data: {
            jobs: query.limit === 100 ? refreshedPage : firstPage,
            has_more: true,
            stats: { done: 101, closed: 0, open: 101 },
          },
          error: undefined,
        };
      }),
    };
  }

  it("preserves the loaded depth during an authoritative refresh", async () => {
    const client = paginatedClient();
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    expect(store.getJobs()).toHaveLength(100);

    await loadJobs(store);

    expect(store.getJobs()).toHaveLength(100);
    expect(client.GET.mock.calls.at(-1)?.[1]?.params.query.limit).toBe(100);
  });

  it("resets the loaded depth when filters change", async () => {
    const client = paginatedClient();
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    store.setFilter("status", "failed");
    await vi.waitFor(() =>
      expect(client.GET.mock.calls.at(-1)?.[1]?.params.query.status).toBe(
        "failed",
      ),
    );

    expect(client.GET.mock.calls.at(-1)?.[1]?.params.query.limit).toBe(50);
  });

  it("keeps subsequent pagination requests at one page", async () => {
    const client = paginatedClient();
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    await loadMoreJobs(store);

    const paginationCalls = client.GET.mock.calls.filter(
      (call) => call[1]?.params.query.before !== undefined,
    );
    expect(paginationCalls).toHaveLength(2);
    expect(paginationCalls[1]?.[1]?.params.query.limit).toBe(50);
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
    const client = {
      GET: vi.fn().mockImplementation(async (_path, options) => {
        const query = options.params.query as {
          before?: number;
          cursor?: string;
        };
        return {
          data: {
            jobs:
              query.cursor === undefined ? [recentlyRerun, olderHighId] : [],
            has_more: query.cursor === undefined,
            next_cursor:
              query.cursor === undefined ? "stable-enqueue-cursor" : null,
            stats: { done: 2, closed: 0, open: 2 },
          },
          error: undefined,
        };
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);

    expect(store.getJobs().map((job) => job.id)).toEqual([1, 100]);
    expect(store.getSortColumn()).toBe("enqueued_at");
    expect(store.canSortJobs()).toBe(false);
    store.setSortColumn("id");
    expect(store.getJobs().map((job) => job.id)).toEqual([1, 100]);

    await loadMoreJobs(store);

    expect(client.GET.mock.calls[1]?.[1]?.params.query.cursor).toBe(
      "stable-enqueue-cursor",
    );
    expect(client.GET.mock.calls[1]?.[1]?.params.query.before).toBeUndefined();
  });

  it("does not load beyond the daemon result limit", async () => {
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: Array.from({ length: 10_000 }, (_, index) =>
            makeJob(10_000 - index),
          ),
          has_more: true,
          stats: { done: 10_001, closed: 0, open: 10_001 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);

    expect(client.GET).toHaveBeenCalledTimes(1);
    expect(store.getHasMore()).toBe(false);
  });

  it("preserves loaded depth when an unchanged filter is reapplied", async () => {
    const client = paginatedClient();
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    store.setFilter("status", undefined);

    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(3));
    expect(client.GET.mock.calls.at(-1)?.[1]?.params.query.limit).toBe(100);
  });

  it("preserves loaded depth when the same repo and branch are reapplied", async () => {
    const client = paginatedClient();
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });

    await loadJobs(store);
    await loadMoreJobs(store);
    store.setRepoBranchFilter(undefined, undefined);

    await vi.waitFor(() => expect(client.GET).toHaveBeenCalledTimes(3));
    expect(client.GET.mock.calls.at(-1)?.[1]?.params.query.limit).toBe(100);
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
    const client = {
      GET: vi
        .fn()
        .mockImplementation(
          (
            _path: string,
            opts: { params: { query: Record<string, unknown> } },
          ) => {
            if (opts.params.query.panel_run === "run-10") {
              return Promise.resolve({
                data: {
                  jobs: [parent, ...members],
                  has_more: false,
                  stats: { done: 0, closed: 0, open: 0 },
                },
                error: undefined,
              });
            }
            return Promise.resolve({
              data: {
                jobs: [parent],
                has_more: false,
                stats: { done: 0, closed: 0, open: 0 },
              },
              error: undefined,
            });
          },
        ),
    };
    const store = createJobsStore({
      client: client as never,
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
    expect(client.GET).toHaveBeenCalledWith(
      "/api/jobs",
      expect.objectContaining({
        params: {
          query: { panel_run: "run-10", limit: 0, omit_prompt: "true" },
        },
      }),
    );

    const calls = client.GET.mock.calls.length;
    store.togglePanel(parent);
    store.togglePanel(parent);
    expect(store.isPanelExpanded("run-10")).toBe(true);
    expect(client.GET.mock.calls.length).toBe(calls);
  });

  it("refreshes members of expanded panels when the listing reloads", async () => {
    const parent = makePanelParent(10);
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [parent],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);
    store.togglePanel(parent);
    await vi.waitFor(() =>
      expect(store.getPanelMembers("run-10")).toBeDefined(),
    );

    const before = client.GET.mock.calls.filter(
      (c) =>
        (c[1] as { params: { query: Record<string, unknown> } }).params.query
          .panel_run === "run-10",
    ).length;
    await loadJobs(store);
    await vi.waitFor(() => {
      const after = client.GET.mock.calls.filter(
        (c) =>
          (c[1] as { params: { query: Record<string, unknown> } }).params.query
            .panel_run === "run-10",
      ).length;
      expect(after).toBe(before + 1);
    });
  });

  it("includes expanded panel members in highlight navigation", async () => {
    const parent = makePanelParent(10);
    const members = [makeMember(12, "run-10", 1), makeMember(11, "run-10", 0)];
    const client = {
      GET: vi
        .fn()
        .mockImplementation(
          (
            _path: string,
            opts: { params: { query: Record<string, unknown> } },
          ) => {
            if (opts.params.query.panel_run === "run-10") {
              return Promise.resolve({
                data: {
                  jobs: [parent, ...members],
                  has_more: false,
                  stats: { done: 0, closed: 0, open: 0 },
                },
                error: undefined,
              });
            }
            return Promise.resolve({
              data: {
                jobs: [parent],
                has_more: false,
                stats: { done: 0, closed: 0, open: 0 },
              },
              error: undefined,
            });
          },
        ),
    };
    const store = createJobsStore({
      client: client as never,
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
      data: {
        jobs: ReviewJob[];
        has_more: boolean;
        stats: { done: number; closed: number; open: number };
      };
      error: undefined;
    }>();
    let panelCalls = 0;
    const client = {
      GET: vi
        .fn()
        .mockImplementation(
          (
            _path: string,
            opts: { params: { query: Record<string, unknown> } },
          ) => {
            if (opts.params.query.panel_run === "run-10") {
              panelCalls++;
              if (panelCalls === 1) {
                return Promise.resolve({
                  data: {
                    jobs: [parent, makeMember(11, "run-10", 0)],
                    has_more: false,
                    stats: { done: 0, closed: 0, open: 0 },
                  },
                  error: undefined,
                });
              }
              return slowRefresh.promise;
            }
            return Promise.resolve({
              data: {
                jobs: [parent],
                has_more: false,
                stats: { done: 0, closed: 0, open: 0 },
              },
              error: undefined,
            });
          },
        ),
    };
    const store = createJobsStore({
      client: client as never,
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
      data: {
        jobs: [parent, makeMember(12, "run-10", 0)],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      },
      error: undefined,
    });
    await vi.waitFor(() => {
      expect(store.getVisibleJobs().map((j) => j.id)).toEqual([10, 12]);
    });
    expect(store.getHighlightedJobId()).toBe(10);
  });

  it("moves highlight to the parent when closing a panel from a member row", async () => {
    const parent = makePanelParent(10);
    const client = {
      GET: vi
        .fn()
        .mockImplementation(
          (
            _path: string,
            opts: { params: { query: Record<string, unknown> } },
          ) => {
            if (opts.params.query.panel_run === "run-10") {
              return Promise.resolve({
                data: {
                  jobs: [parent, makeMember(11, "run-10", 0)],
                  has_more: false,
                  stats: { done: 0, closed: 0, open: 0 },
                },
                error: undefined,
              });
            }
            return Promise.resolve({
              data: {
                jobs: [parent],
                has_more: false,
                stats: { done: 0, closed: 0, open: 0 },
              },
              error: undefined,
            });
          },
        ),
    };
    const store = createJobsStore({
      client: client as never,
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
    const client = {
      GET: vi
        .fn()
        .mockImplementation(
          (
            _path: string,
            opts: { params: { query: Record<string, unknown> } },
          ) => {
            if (opts.params.query.panel_run === "run-10") {
              panelCalls++;
              return Promise.resolve({
                data: {
                  jobs: [
                    parent,
                    makeMember(panelCalls === 1 ? 11 : 12, "run-10", 0),
                  ],
                  has_more: false,
                  stats: { done: 0, closed: 0, open: 0 },
                },
                error: undefined,
              });
            }
            return Promise.resolve({
              data: {
                jobs: [parent],
                has_more: false,
                stats: { done: 0, closed: 0, open: 0 },
              },
              error: undefined,
            });
          },
        ),
    };
    const store = createJobsStore({
      client: client as never,
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
      data: {
        jobs: ReviewJob[];
        has_more: boolean;
        stats: { done: number; closed: number; open: number };
      };
      error: undefined;
    }>();
    let panelCalls = 0;
    const client = {
      GET: vi
        .fn()
        .mockImplementation(
          (
            _path: string,
            opts: { params: { query: Record<string, unknown> } },
          ) => {
            if (opts.params.query.panel_run === "run-10") {
              panelCalls++;
              if (panelCalls === 1) return initialFetch.promise;
              return Promise.resolve({
                data: {
                  jobs: [parent, makeMember(12, "run-10", 0)],
                  has_more: false,
                  stats: { done: 0, closed: 0, open: 0 },
                },
                error: undefined,
              });
            }
            return Promise.resolve({
              data: {
                jobs: [parent],
                has_more: false,
                stats: { done: 0, closed: 0, open: 0 },
              },
              error: undefined,
            });
          },
        ),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);

    store.setPanelMemberInterest("run-10");
    await vi.waitFor(() => expect(panelCalls).toBe(1));
    await loadJobs(store);
    expect(panelCalls).toBe(1);

    initialFetch.resolve({
      data: {
        jobs: [parent, makeMember(11, "run-10", 0)],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      },
      error: undefined,
    });

    await vi.waitFor(() => {
      expect(panelCalls).toBe(2);
      expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([12]);
    });
  });

  it("sorts panel parents by their displayed aggregate cost", async () => {
    const expensive = {
      ...makePanelParent(10),
      token_usage: JSON.stringify({ has_cost: true, cost_usd: 0.01 }),
      panel_summary: {
        ...makePanelParent(10).panel_summary!,
        members_with_cost: 2,
        members_cost_usd: 1,
        members_cost_complete: true,
      },
    };
    const cheaper = {
      ...makePanelParent(20),
      token_usage: JSON.stringify({ has_cost: true, cost_usd: 0.5 }),
      panel_summary: {
        ...makePanelParent(20).panel_summary!,
        members_with_cost: 2,
        members_cost_usd: 0,
        members_cost_complete: true,
      },
    };
    const client = {
      GET: vi.fn().mockResolvedValue({
        data: {
          jobs: [expensive, cheaper],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      }),
    };
    const store = createJobsStore({
      client: client as never,
      navigate: vi.fn(),
    });
    await loadJobs(store);

    store.setSortColumn("cost");

    expect(store.getJobs().map((j) => j.id)).toEqual([20, 10]);
  });

  it("coalesces panel refreshes while a member request is in flight", async () => {
    const parent = makePanelParent(10);
    const duplicateVisibleMember = makeMember(99, "run-10", 1);
    const slowRefresh = deferred<{
      data: {
        jobs: ReviewJob[];
        has_more: boolean;
        stats: { done: number; closed: number; open: number };
      };
      error: undefined;
    }>();
    let panelCalls = 0;
    const client = {
      GET: vi
        .fn()
        .mockImplementation(
          (
            _path: string,
            opts: { params: { query: Record<string, unknown> } },
          ) => {
            if (opts.params.query.panel_run === "run-10") {
              panelCalls++;
              if (panelCalls === 1) {
                return Promise.resolve({
                  data: {
                    jobs: [parent, makeMember(11, "run-10", 0)],
                    has_more: false,
                    stats: { done: 0, closed: 0, open: 0 },
                  },
                  error: undefined,
                });
              }
              if (panelCalls === 2) return slowRefresh.promise;
              return Promise.resolve({
                data: {
                  jobs: [parent, makeMember(13, "run-10", 0)],
                  has_more: false,
                  stats: { done: 0, closed: 0, open: 0 },
                },
                error: undefined,
              });
            }
            return Promise.resolve({
              data: {
                jobs: [parent, duplicateVisibleMember],
                has_more: false,
                stats: { done: 0, closed: 0, open: 0 },
              },
              error: undefined,
            });
          },
        ),
    };
    const store = createJobsStore({
      client: client as never,
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
      data: {
        jobs: [parent, makeMember(12, "run-10", 0)],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      },
      error: undefined,
    });

    await vi.waitFor(() => {
      expect(panelCalls).toBe(3);
      expect(store.getPanelMembers("run-10")?.map((j) => j.id)).toEqual([13]);
    });
  });

  it("keeps accepted members when a stale in-flight refresh is followed by a failed latest refresh", async () => {
    const parent = makePanelParent(10);
    const staleRefresh = deferred<{
      data: {
        jobs: ReviewJob[];
        has_more: boolean;
        stats: { done: number; closed: number; open: number };
      };
      error: undefined;
    }>();
    const onError = vi.fn();
    let panelCalls = 0;
    const client = {
      GET: vi
        .fn()
        .mockImplementation(
          (
            _path: string,
            opts: { params: { query: Record<string, unknown> } },
          ) => {
            if (opts.params.query.panel_run === "run-10") {
              panelCalls++;
              if (panelCalls === 1) {
                return Promise.resolve({
                  data: {
                    jobs: [parent, makeMember(11, "run-10", 0)],
                    has_more: false,
                    stats: { done: 0, closed: 0, open: 0 },
                  },
                  error: undefined,
                });
              }
              if (panelCalls === 2) return staleRefresh.promise;
              return Promise.resolve({
                data: undefined,
                error: { message: "newest refresh failed" },
              });
            }
            return Promise.resolve({
              data: {
                jobs: [parent],
                has_more: false,
                stats: { done: 0, closed: 0, open: 0 },
              },
              error: undefined,
            });
          },
        ),
    };
    const store = createJobsStore({
      client: client as never,
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
      data: {
        jobs: [parent, makeMember(12, "run-10", 0)],
        has_more: false,
        stats: { done: 0, closed: 0, open: 0 },
      },
      error: undefined,
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
