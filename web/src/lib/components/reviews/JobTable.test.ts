import {
  cleanup,
  fireEvent,
  render,
  screen,
  within,
} from "@testing-library/svelte";
import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ListJobsParams, ReviewJob } from "../../api/generated/models";
import type { OwnedAppRuntime } from "../../runtime/runtime";
import {
  createJobsStore,
  type JobsStore,
} from "../../stores/roborev/jobs.svelte";
import { makeTestAppRuntime } from "../../testing/runtime";
import JobTable from "./JobTable.svelte";

const context = vi.hoisted(() => ({
  jobs: undefined as JobsStore | undefined,
}));

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({ roborevJobs: context.jobs }),
}));

// jsdom has no layout, so this observer models the table's scroll area: the
// end sentinel is in view while fewer rows than `rowsThatFit` are rendered, or
// when the user has scrolled to the end and no rows have been added since.
let rowsThatFit = 0;
let rowsAtScrollEnd = -1;
const observers = new Set<FakeIntersectionObserver>();

function renderedRows(): number {
  return document.querySelectorAll(".job-row").length;
}

function renderedIds(): number[] {
  return Array.from(document.querySelectorAll(".job-row .job-id"), (cell) =>
    Number(cell.textContent),
  );
}

function sentinelInView(): boolean {
  const rows = renderedRows();
  return rows < rowsThatFit || rows <= rowsAtScrollEnd;
}

class FakeIntersectionObserver {
  private targets = new Set<Element>();

  constructor(private readonly callback: IntersectionObserverCallback) {}

  observe(target: Element): void {
    this.targets.add(target);
    observers.add(this);
    // Browsers report a newly observed target's position asynchronously.
    queueMicrotask(() => this.report());
  }

  disconnect(): void {
    this.targets.clear();
    observers.delete(this);
  }

  report(): void {
    if (this.targets.size === 0) return;
    const entries = Array.from(
      this.targets,
      (target) =>
        ({
          target,
          isIntersecting: sentinelInView(),
        }) as IntersectionObserverEntry,
    );
    this.callback(entries, this as unknown as IntersectionObserver);
  }
}

function scrollToEnd(): void {
  rowsAtScrollEnd = renderedRows();
  for (const observer of observers) observer.report();
}

function makeJob(id: number): ReviewJob {
  return {
    id,
    agent: "codex",
    agentic: false,
    enqueued_at: new Date(Date.UTC(2026, 3, 11, 0, id)).toISOString(),
    git_ref: `deadbeef${id}`,
    job_type: "review",
    prompt_prebuilt: false,
    repo_id: 1,
    retry_count: 0,
    status: "done",
  };
}

interface JobsPage {
  jobs: ReviewJob[];
  has_more: boolean;
  next_cursor: string | null;
  stats: { done: number; closed: number; open: number };
}

// Serves `pageCount` pages of `pageSize` jobs, newest first, linked by cursor.
function pagedJobs(pageSize: number, pageCount: number) {
  const total = pageSize * pageCount;
  return (query: ListJobsParams): JobsPage => {
    const page = query.cursor === undefined ? 0 : Number(query.cursor);
    const newest = total - page * pageSize;
    const hasMore = page + 1 < pageCount;
    return {
      jobs: Array.from({ length: pageSize }, (_, index) =>
        makeJob(newest - index),
      ),
      has_more: hasMore,
      next_cursor: hasMore ? String(page + 1) : null,
      stats: { done: total, closed: 0, open: total },
    };
  };
}

function isPageRequest(query: ListJobsParams): boolean {
  return query.cursor !== undefined;
}

let runtime: OwnedAppRuntime;

function mountTable(
  listJobs: (query: ListJobsParams) => Promise<JobsPage>,
): ReturnType<typeof vi.fn> {
  const api = vi.fn(listJobs);
  const store = createJobsStore({
    api: { listJobs: api, cancelJob: vi.fn(), rerunJob: vi.fn() } as never,
    runtime,
    owner: "job-table-test",
    navigate: vi.fn(),
  });
  context.jobs = store;
  render(JobTable);
  store.loadJobs();
  return api;
}

function pageRequests(api: ReturnType<typeof vi.fn>): ListJobsParams[] {
  return api.mock.calls
    .map((call) => call[0] as ListJobsParams)
    .filter(isPageRequest);
}

async function settle(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 20));
}

describe("JobTable infinite scroll", () => {
  beforeEach(() => {
    localStorage.clear();
    rowsThatFit = 0;
    rowsAtScrollEnd = -1;
    vi.stubGlobal("IntersectionObserver", FakeIntersectionObserver);
    runtime = makeTestAppRuntime();
  });

  afterEach(async () => {
    cleanup();
    context.jobs?.dispose();
    context.jobs = undefined;
    observers.clear();
    vi.unstubAllGlobals();
    await Effect.runPromise(runtime.disposeEffect);
  });

  it("loads the next page when the table scrolls to its end", async () => {
    rowsThatFit = 5;
    const serve = pagedJobs(10, 3);
    const api = mountTable(async (query) => serve(query));

    await vi.waitFor(() => expect(renderedRows()).toBe(10));
    await settle();
    expect(pageRequests(api)).toEqual([]);

    scrollToEnd();

    await vi.waitFor(() => expect(renderedRows()).toBe(20));
    await settle();
    expect(pageRequests(api)).toHaveLength(1);
    expect(pageRequests(api)[0]?.cursor).toBe("1");
  });

  it("sends one request while a page is loading", async () => {
    rowsThatFit = 5;
    const serve = pagedJobs(10, 2);
    const pending = Promise.withResolvers<void>();
    const api = mountTable(async (query) => {
      if (isPageRequest(query)) await pending.promise;
      return serve(query);
    });
    await vi.waitFor(() => expect(renderedRows()).toBe(10));

    scrollToEnd();
    scrollToEnd();
    await vi.waitFor(() => expect(pageRequests(api)).toHaveLength(1));
    scrollToEnd();
    await settle();
    expect(pageRequests(api)).toHaveLength(1);
    expect(screen.getByRole("status")).toHaveTextContent("Loading...");

    pending.resolve();

    await vi.waitFor(() => expect(renderedRows()).toBe(20));
    expect(pageRequests(api)).toHaveLength(1);
  });

  it("stops requesting pages after the last one", async () => {
    rowsThatFit = 5;
    const serve = pagedJobs(10, 2);
    const api = mountTable(async (query) => serve(query));
    await vi.waitFor(() => expect(renderedRows()).toBe(10));

    scrollToEnd();
    await vi.waitFor(() => expect(renderedRows()).toBe(20));
    scrollToEnd();
    await settle();

    expect(pageRequests(api)).toHaveLength(1);
    expect(document.querySelector(".scroll-sentinel")).toBeNull();
  });

  it("keeps loading until the rows fill the scroll area", async () => {
    rowsThatFit = 25;
    const serve = pagedJobs(10, 5);
    const api = mountTable(async (query) => serve(query));

    await vi.waitFor(() => expect(renderedRows()).toBe(30));
    await settle();

    expect(renderedRows()).toBe(30);
    expect(pageRequests(api).map((query) => query.cursor)).toEqual(["1", "2"]);
  });

  it("shows a failed page with a retry instead of retrying on scroll", async () => {
    rowsThatFit = 5;
    const serve = pagedJobs(10, 2);
    let failPages = true;
    const api = mountTable(async (query) => {
      if (isPageRequest(query) && failPages) {
        throw new Error("daemon unavailable");
      }
      return serve(query);
    });
    await vi.waitFor(() => expect(renderedRows()).toBe(10));

    scrollToEnd();
    const failure = await screen.findByRole("alert");
    expect(failure).toHaveTextContent("Could not load more jobs.");
    scrollToEnd();
    await settle();
    expect(pageRequests(api)).toHaveLength(1);

    failPages = false;
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await vi.waitFor(() => expect(renderedRows()).toBe(20));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(pageRequests(api)).toHaveLength(2);
  });

  it("resumes paging after a filter change replaces an in-flight page", async () => {
    rowsThatFit = 5;
    const serve = pagedJobs(10, 3);
    const stalled = Promise.withResolvers<void>();
    let stallPages = true;
    const api = mountTable(async (query) => {
      if (isPageRequest(query) && stallPages) await stalled.promise;
      return serve(query);
    });
    await vi.waitFor(() => expect(renderedRows()).toBe(10));
    scrollToEnd();
    await vi.waitFor(() => expect(pageRequests(api)).toHaveLength(1));

    stallPages = false;
    context.jobs?.setFilter("status", "done");
    await vi.waitFor(() =>
      expect(
        api.mock.calls.some(
          (call) =>
            (call[0] as ListJobsParams).status === "done" &&
            !isPageRequest(call[0] as ListJobsParams),
        ),
      ).toBe(true),
    );

    // The view is still at the end, so the replaced list pages again.
    await vi.waitFor(() => expect(renderedRows()).toBe(20));
    expect(pageRequests(api).at(-1)).toMatchObject({
      status: "done",
      cursor: "1",
    });
  });

  it("lists scroll-loaded pages in the order of the chosen sort", async () => {
    rowsThatFit = 5;
    const serve = pagedJobs(10, 3);
    const api = mountTable(async (query) => {
      if (query.sort !== "id" || query.direction !== "asc") return serve(query);
      // Oldest-first by ID: 1..10, then 11..20 after cursor "asc-1".
      const second = query.cursor === "asc-1";
      return {
        jobs: Array.from({ length: 10 }, (_, index) =>
          makeJob((second ? 11 : 1) + index),
        ),
        has_more: !second,
        next_cursor: second ? null : "asc-1",
        stats: { done: 20, closed: 0, open: 20 },
      };
    });
    await vi.waitFor(() => expect(renderedRows()).toBe(10));
    const idButton = within(
      screen.getByRole("columnheader", { name: "ID" }),
    ).getByRole("button");

    await fireEvent.click(idButton);
    await fireEvent.click(idButton);
    await vi.waitFor(() => expect(renderedIds()[0]).toBe(1));
    scrollToEnd();

    await vi.waitFor(() => expect(renderedRows()).toBe(20));
    expect(renderedIds()).toEqual(
      Array.from({ length: 20 }, (_, index) => index + 1),
    );
    expect(pageRequests(api).at(-1)).toMatchObject({
      cursor: "asc-1",
      sort: "id",
      direction: "asc",
    });
  });
});

describe("JobTable sort reload", () => {
  beforeEach(() => {
    localStorage.clear();
    rowsThatFit = 0;
    rowsAtScrollEnd = -1;
    vi.stubGlobal("IntersectionObserver", FakeIntersectionObserver);
    runtime = makeTestAppRuntime();
  });

  afterEach(async () => {
    cleanup();
    context.jobs?.dispose();
    context.jobs = undefined;
    observers.clear();
    vi.unstubAllGlobals();
    await Effect.runPromise(runtime.disposeEffect);
  });

  // Newest first by default; oldest first by ID once the ID header is
  // clicked twice (descending, then ascending).
  function serveBySort(query: ListJobsParams): JobsPage {
    const ascending = query.sort === "id" && query.direction === "asc";
    return {
      jobs: [3, 2, 1].map((id) => makeJob(ascending ? 4 - id : id)),
      has_more: false,
      next_cursor: null,
      stats: { done: 3, closed: 0, open: 3 },
    };
  }

  function idHeaderButton(): HTMLElement {
    return within(screen.getByRole("columnheader", { name: "ID" })).getByRole(
      "button",
    );
  }

  function headerGroup(): HTMLElement {
    const group = screen.getByRole("table").querySelector("thead");
    if (group === null) throw new Error("job table has no header");
    return group;
  }

  it("marks the rows busy until the new order arrives", async () => {
    const reply = Promise.withResolvers<void>();
    mountTable(async (query) => {
      if (query.sort === "id") await reply.promise;
      return serveBySort(query);
    });
    await vi.waitFor(() => expect(renderedIds()).toEqual([3, 2, 1]));
    const table = screen.getByRole("table");
    expect(table).toHaveAttribute("aria-busy", "false");

    await fireEvent.click(idHeaderButton());

    expect(table).toHaveAttribute("aria-busy", "true");
    reply.resolve();
    await vi.waitFor(() => expect(table).toHaveAttribute("aria-busy", "false"));
  });

  it("shows a failed sort under the header and clears it after a reload", async () => {
    let failSort = true;
    mountTable(async (query) => {
      if (query.sort === "id" && failSort) {
        throw new Error("daemon unavailable");
      }
      return serveBySort(query);
    });
    await vi.waitFor(() => expect(renderedIds()).toEqual([3, 2, 1]));

    await fireEvent.click(idHeaderButton());
    await fireEvent.click(idHeaderButton());

    const failure = await within(headerGroup()).findByRole("alert");
    expect(failure).toHaveTextContent("Could not sort jobs.");
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(screen.getByRole("table")).toHaveAttribute("aria-busy", "false");
    expect(renderedIds()).toEqual([3, 2, 1]);

    failSort = false;
    await fireEvent.click(
      within(failure).getByRole("button", { name: "Retry" }),
    );

    await vi.waitFor(() => expect(renderedIds()).toEqual([1, 2, 3]));
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
