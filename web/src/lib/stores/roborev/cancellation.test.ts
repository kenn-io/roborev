import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { OwnedAppRuntime } from "../../runtime/runtime";
import { makeTestAppRuntime } from "../../testing/runtime";
import { createJobsStore } from "./jobs.svelte";
import { createReviewStore } from "./review.svelte";

let runtime: OwnedAppRuntime | undefined;

function reviewStore(
  client: Parameters<typeof createReviewStore>[0]["client"],
  onError?: (message: string) => void,
) {
  if (runtime === undefined)
    throw new Error("test runtime was not initialized");
  return createReviewStore({
    client,
    runtime,
    owner: "review-cancellation-test",
    ...(onError !== undefined && { onError }),
  });
}

beforeEach(() => {
  runtime = makeTestAppRuntime();
});

afterEach(async () => {
  if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
  runtime = undefined;
});

function abortingGet() {
  return vi.fn(
    (_path: string, options?: { signal?: AbortSignal }) =>
      new Promise<never>((_resolve, reject) => {
        options?.signal?.addEventListener(
          "abort",
          () => reject(new DOMException("request aborted", "AbortError")),
          {
            once: true,
          },
        );
      }),
  );
}

describe("Roborev request cancellation", () => {
  it("aborts stale review transport when another job becomes authoritative", async () => {
    const staleSignals: AbortSignal[] = [];
    const get = vi.fn(
      (
        path: string,
        options?: {
          params?: { query?: { job_id?: number } };
          signal?: AbortSignal;
        },
      ) => {
        const jobID = options?.params?.query?.job_id;
        if (jobID === 42) {
          if (options?.signal !== undefined) staleSignals.push(options.signal);
          return new Promise<never>((_resolve, reject) => {
            options?.signal?.addEventListener(
              "abort",
              () => reject(new DOMException("request aborted", "AbortError")),
              {
                once: true,
              },
            );
          });
        }
        if (path === "/api/review") {
          return Promise.resolve({
            data: {
              id: 7,
              job_id: 43,
              output: "current review",
              closed: false,
            },
            error: undefined,
          });
        }
        if (path === "/api/ui/review-projection") {
          return Promise.resolve({ data: { responses: [] }, error: undefined });
        }
        return Promise.resolve({
          data: {
            jobs: [],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          },
          error: undefined,
        });
      },
    );
    const store = reviewStore({ GET: get } as never);

    store.setSelectedJobId(42);
    void store.loadReview(42);
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(3));

    store.setSelectedJobId(43);
    store.loadReview(43);
    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(43));

    expect(staleSignals).toHaveLength(2);
    expect(staleSignals.every((signal) => signal.aborted)).toBe(true);
    expect(store.getReview()?.job_id).toBe(43);
  });

  it("does not publish a review error after its owner clears the selection", async () => {
    const onError = vi.fn();
    const get = abortingGet();
    const store = reviewStore({ GET: get } as never, onError);

    store.setSelectedJobId(42);
    store.loadReview(42);
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(3));
    store.setSelectedJobId(undefined);
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));

    expect(store.getError()).toBeNull();
    expect(store.isLoading()).toBe(false);
    expect(onError).not.toHaveBeenCalled();
  });

  it("loads merged historical comments from the review projection", async () => {
    const get = vi.fn((path: string) => {
      if (path === "/api/review") {
        return Promise.resolve({
          data: {
            id: 7,
            job_id: 42,
            output: "review output",
            prompt: "review prompt",
            closed: false,
          },
          error: undefined,
        });
      }
      if (path === "/api/comments") {
        return Promise.resolve({
          data: {
            responses: [
              {
                id: 2,
                created_at: "2026-08-14T12:02:00Z",
                responder: "reviewer-b",
                response: "Job-linked response",
                job_id: 42,
              },
            ],
          },
          error: undefined,
        });
      }
      if (path === "/api/ui/review-projection") {
        return Promise.resolve({
          data: {
            responses: [
              {
                id: 1,
                created_at: "2026-08-14T12:01:00Z",
                responder: "reviewer-a",
                response: "Historical commit response",
              },
              {
                id: 2,
                created_at: "2026-08-14T12:02:00Z",
                responder: "reviewer-b",
                response: "Job-linked response",
              },
            ],
          },
          error: undefined,
        });
      }
      return Promise.resolve({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      });
    });
    const store = reviewStore({ GET: get } as never);

    store.setSelectedJobId(42);
    store.loadReview(42);

    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(42));
    expect(store.getResponses().map((response) => response.id)).toEqual([1, 2]);
    expect(get.mock.calls.map(([path]) => path)).not.toContain("/api/comments");
  });

  it("uses the selected job prompt when no completed review exists", async () => {
    const get = vi.fn((path: string) => {
      if (path === "/api/review" || path === "/api/ui/review-projection") {
        return Promise.resolve({
          data: undefined,
          error: { detail: "not found" },
          response: { status: 404 },
        });
      }
      return Promise.resolve({
        data: {
          jobs: [
            {
              id: 42,
              repo_id: 1,
              git_ref: "abc123",
              agent: "test",
              job_type: "review",
              status: "failed",
              enqueued_at: "2026-08-14T12:00:00Z",
              prompt: "persisted review prompt",
              retry_count: 0,
              agentic: false,
              prompt_prebuilt: false,
            },
          ],
          has_more: false,
        },
        error: undefined,
      });
    });
    const store = reviewStore({ GET: get } as never);

    store.setSelectedJobId(42);
    store.loadReview(42);

    await vi.waitFor(() => expect(store.isLoading()).toBe(false));
    expect(store.getPrompt()).toBe("persisted review prompt");
  });

  it("aborts stale job-list transport when a newer list becomes authoritative", async () => {
    const staleSignals: AbortSignal[] = [];
    let calls = 0;
    const get = vi.fn((_path: string, options?: { signal?: AbortSignal }) => {
      calls += 1;
      if (calls === 1) {
        if (options?.signal !== undefined) staleSignals.push(options.signal);
        return new Promise<never>((_resolve, reject) => {
          options?.signal?.addEventListener(
            "abort",
            () => reject(new DOMException("request aborted", "AbortError")),
            {
              once: true,
            },
          );
        });
      }
      return Promise.resolve({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      });
    });
    const store = createJobsStore({
      client: { GET: get } as never,
      runtime: runtime!,
      owner: "jobs-latest-test",
      navigate: vi.fn(),
    });

    store.loadJobs();
    await vi.waitFor(() => expect(get).toHaveBeenCalledOnce());
    store.loadJobs();
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));

    expect(staleSignals).toHaveLength(1);
    expect(staleSignals.every((signal) => signal.aborted)).toBe(true);
    expect(get).toHaveBeenCalledTimes(2);
    expect(store.getError()).toBeNull();
  });

  it("keeps an accepted close bound to its submitted job when a queued comment settles after navigation", async () => {
    const commentResponse = Promise.withResolvers<{
      data: {
        id: number;
        created_at: string;
        responder: string;
        response: string;
        job_id: number;
      };
      error: undefined;
    }>();
    let job42Closed = false;
    const closeBodies: Array<{ job_id: number; closed: boolean }> = [];
    const get = vi.fn(
      (
        path: string,
        options?: { params?: { query?: { job_id?: number; id?: number } } },
      ) => {
        const jobID =
          options?.params?.query?.job_id ?? options?.params?.query?.id ?? 42;
        if (path === "/api/review") {
          return Promise.resolve({
            data: {
              id: jobID,
              job_id: jobID,
              agent: "codex",
              created_at: "2026-08-04T12:00:00Z",
              output: `review ${jobID}`,
              prompt: "review",
              closed: jobID === 42 ? job42Closed : true,
            },
            error: undefined,
          });
        }
        if (path === "/api/ui/review-projection") {
          return Promise.resolve({ data: { responses: [] }, error: undefined });
        }
        return Promise.resolve({
          data: {
            jobs: [],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          },
          error: undefined,
        });
      },
    );
    const post = vi.fn(
      (
        path: string,
        options: { body: { job_id: number; closed?: boolean } },
      ) => {
        if (path === "/api/comment") return commentResponse.promise;
        const body = {
          job_id: options.body.job_id,
          closed: options.body.closed ?? false,
        };
        closeBodies.push(body);
        job42Closed = body.closed;
        return Promise.resolve({ data: { success: true }, error: undefined });
      },
    );
    const store = reviewStore({ GET: get, POST: post } as never);

    store.setSelectedJobId(42);
    store.loadReview(42);
    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(42));
    store.addComment(42, "hold the mutation queue");
    await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(1));
    store.closeReview(42);

    store.setSelectedJobId(43);
    store.loadReview(43);
    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(43));
    commentResponse.resolve({
      data: {
        id: 1,
        created_at: "2026-08-04T12:01:00Z",
        responder: "web",
        response: "hold the mutation queue",
        job_id: 42,
      },
      error: undefined,
    });

    await vi.waitFor(() => expect(closeBodies).toHaveLength(1));
    expect(closeBodies[0]).toEqual({ job_id: 42, closed: true });
    expect(store.getReview()?.job_id).toBe(43);
    expect(store.isClosed()).toBe(true);
    expect(store.getResponses()).toEqual([]);
  });

  it("reconciles a lost comment response without replaying the comment", async () => {
    let commentCommitted = false;
    const acceptedComment = {
      id: 9,
      created_at: "2026-08-04T12:01:00Z",
      responder: "web",
      response: "retain this comment",
      job_id: 42,
    };
    const get = vi.fn((path: string) => {
      if (path === "/api/review") {
        return Promise.resolve({
          data: {
            id: 42,
            job_id: 42,
            agent: "codex",
            created_at: "2026-08-04T12:00:00Z",
            output: "review",
            prompt: "review",
            closed: false,
          },
          error: undefined,
        });
      }
      if (path === "/api/ui/review-projection") {
        return Promise.resolve({
          data: { responses: commentCommitted ? [acceptedComment] : [] },
          error: undefined,
        });
      }
      return Promise.resolve({
        data: {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        },
        error: undefined,
      });
    });
    const post = vi.fn().mockImplementation(() => {
      commentCommitted = true;
      return Promise.reject(new TypeError("response lost"));
    });
    const store = reviewStore({ GET: get, POST: post } as never);
    const onSuccess = vi.fn();
    const onFailure = vi.fn();

    store.setSelectedJobId(42);
    store.loadReview(42);
    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(42));
    store.addComment(42, "retain this comment", { onSuccess, onFailure });

    await vi.waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
    expect(onFailure).not.toHaveBeenCalled();
    expect(post).toHaveBeenCalledTimes(1);
    expect(store.getResponses().map((response) => response.id)).toEqual([9]);
  });
});
