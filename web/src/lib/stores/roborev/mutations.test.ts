import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cancelJob, listJobs, rerunJob } from "../../api/generated/jobs/jobs";
import type { RoborevRequestOptions } from "../../api/generated-fetch";
import type { Fetch } from "../../api/session";
import { makeAppRuntime, type OwnedAppRuntime } from "../../runtime/runtime";
import { createJobsStore } from "./jobs.svelte";

let runtime: OwnedAppRuntime;

beforeEach(() => {
  runtime = makeAppRuntime();
});

afterEach(async () => {
  await Effect.runPromise(runtime.disposeEffect);
});

function generatedJobsApi(fetch: Fetch) {
  const transport = { baseUrl: "http://localhost", fetch };
  const options = (request?: RequestInit): RoborevRequestOptions => ({
    ...request,
    roborevTransport: transport,
  });
  return {
    cancelJob: (request: Parameters<typeof cancelJob>[0], init?: RequestInit) =>
      cancelJob(request, options(init)),
    listJobs: (query?: Parameters<typeof listJobs>[0], init?: RequestInit) =>
      listJobs(query, options(init)),
    rerunJob: (request: Parameters<typeof rerunJob>[0], init?: RequestInit) =>
      rerunJob(request, options(init)),
  };
}

describe("Roborev mutation ownership", () => {
  it("coalesces duplicate reruns while the first request is pending", async () => {
    const rerun = Promise.withResolvers<{
      success: boolean;
      job_id: number;
      request_id: string;
    }>();
    const rerunJob = vi.fn().mockReturnValue(rerun.promise);
    const listJobs = vi.fn().mockResolvedValue({
      jobs: [{ id: 17, retry_count: 0, status: "queued" }],
      has_more: false,
      stats: { queued: 1, done: 0, closed: 0, open: 1 },
    });
    const store = createJobsStore({
      api: { listJobs, rerunJob } as never,
      runtime,
      owner: "rerun-coalescing-test",
      navigate: vi.fn(),
    });

    store.rerunJob(17);
    store.rerunJob(17);

    await vi.waitFor(() => expect(rerunJob).toHaveBeenCalledOnce());
    expect(store.isRerunning(17)).toBe(true);
    rerun.resolve({
      success: true,
      job_id: 17,
      request_id: "request-one",
    });
    await vi.waitFor(() => expect(store.isRerunning(17)).toBe(false));
  });

  it("retries an ambiguous rerun with one request ID and follows the returned job", async () => {
    const requestIDs: string[] = [];
    let applied = false;
    const listJobs = vi.fn().mockImplementation((query) => {
      const id = query.id;
      return Promise.resolve({
        jobs:
          id === 99 && applied
            ? [{ id: 99, retry_count: 0, status: "queued" }]
            : [{ id: 17, retry_count: 0, status: "done" }],
        has_more: false,
        stats: { queued: applied ? 1 : 0, done: applied ? 0 : 1 },
      });
    });
    const rerunJob = vi.fn().mockImplementation((request) => {
      requestIDs.push(request.request_id);
      if (!applied) {
        applied = true;
        return Promise.reject(new TypeError("response lost"));
      }
      return Promise.resolve({
        success: true,
        job_id: 99,
        request_id: request.request_id,
        run_uuid: "replacement-panel",
      });
    });
    const navigate = vi.fn();
    const store = createJobsStore({
      api: { listJobs, rerunJob } as never,
      runtime,
      owner: "rerun-idempotency-test",
      navigate,
    });

    const execution = runtime.runCommand(store.rerunJobEffect(17), {
      operation: "rerun Roborev job",
      safeContext: {},
      onFailure: vi.fn(),
    });
    await Effect.runPromise(execution.await);

    expect(rerunJob).toHaveBeenCalledTimes(2);
    expect(requestIDs[0]).toBeTruthy();
    expect(new Set(requestIDs).size).toBe(1);
    expect(navigate).toHaveBeenCalledWith(99);
  });

  it("lets later mutations run after a rerun request is rejected", async () => {
    const posts: string[] = [];
    const fetchFn: Fetch = (input, init) => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      if (request.method === "POST") {
        posts.push(url.pathname);
        if (url.pathname === "/api/job/rerun") {
          return Promise.resolve(
            Response.json({ detail: "not rerunnable" }, { status: 404 }),
          );
        }
        return Promise.resolve(Response.json({ success: true }));
      }
      return Promise.resolve(
        Response.json({
          jobs: [
            {
              id: Number(url.searchParams.get("id") ?? 18),
              agent: "codex",
              agentic: false,
              enqueued_at: "2026-08-04T12:00:00Z",
              git_ref: "deadbeef",
              job_type: "review",
              prompt_prebuilt: false,
              repo_id: 1,
              retry_count: 0,
              status: "canceled",
            },
          ],
          has_more: false,
          stats: { done: 1, closed: 0, open: 0 },
        }),
      );
    };
    const errors: string[] = [];
    const store = createJobsStore({
      api: generatedJobsApi(fetchFn),
      runtime,
      owner: "rerun-preflight-failure-test",
      navigate: vi.fn(),
      onError: (message) => errors.push(message),
    });

    store.rerunJob(17);
    store.cancelJob(18);

    await vi.waitFor(() => expect(posts).toContain("/api/job/cancel"));
    expect(posts).toContain("/api/job/rerun");
    expect(errors).toContain("Failed to rerun job");
  });

  it("revalidates the stable rerun result returned by the daemon", async () => {
    const events: string[] = [];
    let rerunApplied = false;
    const listJobs = vi.fn().mockImplementation((query) => {
      events.push(`get:${query.id}`);
      return Promise.resolve({
        jobs: [
          {
            id: 17,
            agent: "codex",
            agentic: false,
            enqueued_at: "2026-08-04T12:00:00Z",
            git_ref: "deadbeef",
            job_type: "review",
            prompt_prebuilt: false,
            repo_id: 1,
            retry_count: 0,
            status: rerunApplied ? "queued" : "done",
          },
        ],
        has_more: false,
        stats: {
          done: rerunApplied ? 0 : 1,
          closed: 0,
          open: rerunApplied ? 1 : 0,
        },
      });
    });
    const rerunJob = vi.fn().mockImplementation((request) => {
      events.push("post");
      rerunApplied = true;
      return Promise.resolve({
        success: true,
        job_id: 17,
        request_id: request.request_id,
      });
    });
    const store = createJobsStore({
      api: { listJobs, rerunJob } as never,
      runtime,
      owner: "rerun-baseline-test",
      navigate: vi.fn(),
    });
    store.setSelectedJobId(17);
    const reviewRevision = store.getSelectedReviewRevision();

    store.rerunJob(17);

    await vi.waitFor(() => expect(rerunJob).toHaveBeenCalledOnce());
    await vi.waitFor(() => expect(events).toContain("get:17"));
    await vi.waitFor(() => expect(store.isRerunning(17)).toBe(false));
    expect(events.slice(0, 2)).toEqual(["post", "get:17"]);
    expect(store.getSelectedReviewRevision()).toBe(reviewRevision + 1);
  });

  it("keeps a delayed rerun request ordered ahead of later actions", async () => {
    const rerun = Promise.withResolvers<{
      success: boolean;
      job_id: number;
      request_id: string;
    }>();
    const posts: string[] = [];
    const listJobs = vi.fn().mockImplementation((query) => {
      return Promise.resolve({
        jobs: [{ id: query.id, retry_count: 1, status: "canceled" }],
      });
    });
    const rerunJob = vi.fn().mockImplementation(() => {
      posts.push("rerun");
      return rerun.promise;
    });
    const cancelJob = vi.fn().mockImplementation(() => {
      posts.push("cancel");
      return Promise.resolve({ success: true });
    });
    const store = createJobsStore({
      api: { cancelJob, listJobs, rerunJob } as never,
      runtime,
      owner: "rerun-preflight-order-test",
      navigate: vi.fn(),
    });

    store.rerunJob(17);
    store.cancelJob(17);
    await vi.waitFor(() => expect(posts).toEqual(["rerun"]));

    rerun.resolve({
      success: true,
      job_id: 17,
      request_id: "request-one",
    });
    await vi.waitFor(() => expect(posts).toEqual(["rerun", "cancel"]));
  });

  it("does not start a later rerun before an accepted cancellation settles", async () => {
    const first = Promise.withResolvers<{
      success: boolean;
    }>();
    const requests: string[] = [];
    const fetchFn: Fetch = (input) => {
      const path = new URL(new Request(input).url).pathname;
      requests.push(path);
      if (path === "/api/job/cancel")
        return first.promise.then((result) => Response.json(result));
      if (path === "/api/jobs") {
        return Promise.resolve(
          Response.json({
            jobs: [
              {
                id: 17,
                agent: "codex",
                agentic: false,
                enqueued_at: "2026-08-04T12:00:00Z",
                git_ref: "deadbeef",
                job_type: "review",
                prompt_prebuilt: false,
                repo_id: 1,
                retry_count: 0,
                status: "canceled",
              },
            ],
            has_more: false,
            stats: { done: 1, closed: 0, open: 0 },
          }),
        );
      }
      return Promise.resolve(Response.json({ success: true }));
    };
    const store = createJobsStore({
      api: generatedJobsApi(fetchFn),
      runtime,
      owner: "mutation-test",
      navigate: vi.fn(),
    });

    store.cancelJob(17);
    await vi.waitFor(() => expect(requests).toHaveLength(1));
    store.rerunJob(17);
    await Promise.resolve();

    expect(
      requests.filter(
        (path) => path === "/api/job/cancel" || path === "/api/job/rerun",
      ),
    ).toHaveLength(1);
    first.resolve({ success: true });
    await vi.waitFor(() => expect(requests).toContain("/api/job/rerun"));
    expect(
      requests.filter(
        (path) => path === "/api/job/cancel" || path === "/api/job/rerun",
      ),
    ).toEqual(["/api/job/cancel", "/api/job/rerun"]);
  });

  it("revalidates job rows and aggregate stats after a cancellation acknowledgement", async () => {
    let canceled = false;
    const listJobs = vi.fn().mockImplementation(() =>
      Promise.resolve({
        jobs: [
          {
            id: 17,
            agent: "codex",
            agentic: false,
            enqueued_at: "2026-08-04T12:00:00Z",
            git_ref: "deadbeef",
            job_type: "review",
            prompt_prebuilt: false,
            repo_id: 1,
            retry_count: 0,
            status: canceled ? "canceled" : "running",
          },
        ],
        has_more: false,
        stats: canceled
          ? { done: 1, closed: 0, open: 0 }
          : { done: 0, closed: 0, open: 1 },
      }),
    );
    const cancelJob = vi.fn().mockImplementation(() => {
      canceled = true;
      return Promise.resolve({ success: true });
    });
    const store = createJobsStore({
      api: { cancelJob, listJobs } as never,
      runtime,
      owner: "mutation-revalidation-test",
      navigate: vi.fn(),
    });
    store.setSelectedJobId(17);
    const reviewRevision = store.getSelectedReviewRevision();
    const initial = runtime.runCommand(store.loadJobsEffect(), {
      operation: "load Roborev jobs",
      safeContext: {},
      onFailure: () => {},
    });
    await Effect.runPromise(initial.await);

    store.cancelJob(17);

    await vi.waitFor(() =>
      expect(store.getStats()).toEqual({ done: 1, closed: 0, open: 0 }),
    );
    expect(store.getJobs()[0]?.status).toBe("canceled");
    expect(listJobs.mock.calls.length).toBeGreaterThan(2);
    expect(store.getSelectedReviewRevision()).toBe(reviewRevision + 1);
  });
});
