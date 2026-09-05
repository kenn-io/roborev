import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { OwnedAppRuntime } from "../../runtime/runtime";
import { makeTestAppRuntime } from "../../testing/runtime";
import { createJobsStore } from "./jobs.svelte";
import { createReviewStore } from "./review.svelte";

let runtime: OwnedAppRuntime | undefined;

function reviewStore(
  api: Parameters<typeof createReviewStore>[0]["api"],
  onError?: (message: string) => void,
) {
  if (runtime === undefined)
    throw new Error("test runtime was not initialized");
  return createReviewStore({
    api,
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

function abortingRequest() {
  return vi.fn(
    (_query: unknown, options?: { signal?: AbortSignal }) =>
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
    const request = <A>(value: A) =>
      vi.fn(
        (query?: { job_id?: number }, options?: { signal?: AbortSignal }) => {
          const jobID = query?.job_id;
          if (jobID === 42) {
            if (options?.signal !== undefined)
              staleSignals.push(options.signal);
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
          return Promise.resolve(value);
        },
      );
    const getReview = request({
      id: 7,
      job_id: 43,
      output: "current review",
      closed: false,
    });
    const getReviewProjection = request({ responses: [] });
    const listJobs = request({
      jobs: [],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });
    const store = reviewStore({
      getReview,
      getReviewProjection,
      listJobs,
    } as never);

    store.setSelectedJobId(42);
    void store.loadReview(42);
    await vi.waitFor(() => expect(staleSignals).toHaveLength(2));

    store.setSelectedJobId(43);
    store.loadReview(43);
    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(43));

    expect(staleSignals).toHaveLength(2);
    expect(staleSignals.every((signal) => signal.aborted)).toBe(true);
    expect(store.getReview()?.job_id).toBe(43);
  });

  it("does not publish a review error after its owner clears the selection", async () => {
    const onError = vi.fn();
    const getReview = abortingRequest();
    const getReviewProjection = abortingRequest();
    const listJobs = abortingRequest();
    const store = reviewStore(
      { getReview, getReviewProjection, listJobs } as never,
      onError,
    );

    store.setSelectedJobId(42);
    store.loadReview(42);
    await vi.waitFor(() => expect(getReview).toHaveBeenCalledOnce());
    store.setSelectedJobId(undefined);
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));

    expect(store.getError()).toBeNull();
    expect(store.isLoading()).toBe(false);
    expect(onError).not.toHaveBeenCalled();
  });

  it("loads merged historical comments from the review projection", async () => {
    const getReview = vi.fn().mockResolvedValue({
      id: 7,
      job_id: 42,
      output: "review output",
      prompt: "review prompt",
      closed: false,
    });
    const getReviewProjection = vi.fn().mockResolvedValue({
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
    });
    const listJobs = vi.fn().mockResolvedValue({
      jobs: [],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });
    const store = reviewStore({
      getReview,
      getReviewProjection,
      listJobs,
    } as never);

    store.setSelectedJobId(42);
    store.loadReview(42);

    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(42));
    expect(store.getResponses().map((response) => response.id)).toEqual([1, 2]);
    expect(getReviewProjection).toHaveBeenCalledOnce();
  });

  it("uses the selected job prompt and projection when no review exists", async () => {
    const notFound = () => Promise.reject(new Response(null, { status: 404 }));
    const response = {
      id: 7,
      created_at: "2026-08-14T12:01:00Z",
      responder: "web",
      response: "Queued job comment",
    };
    const getReviewProjection = vi.fn().mockResolvedValue({
      responses: [response],
    });
    const listJobs = vi.fn().mockResolvedValue({
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
    });
    const store = reviewStore({
      getReview: notFound,
      getReviewProjection,
      listJobs,
    } as never);

    store.setSelectedJobId(42);
    store.loadReview(42);

    await vi.waitFor(() => expect(store.isLoading()).toBe(false));
    expect(store.getPrompt()).toBe("persisted review prompt");
    expect(store.getResponses()).toEqual([response]);
  });

  it("aborts stale job-list transport when a newer list becomes authoritative", async () => {
    const staleSignals: AbortSignal[] = [];
    let calls = 0;
    const listJobs = vi.fn(
      (_query: unknown, options?: { signal?: AbortSignal }) => {
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
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        });
      },
    );
    const store = createJobsStore({
      api: { listJobs } as never,
      runtime: runtime!,
      owner: "jobs-latest-test",
      navigate: vi.fn(),
    });

    store.loadJobs();
    await vi.waitFor(() => expect(listJobs).toHaveBeenCalledOnce());
    store.loadJobs();
    await vi.waitFor(() => expect(store.isLoading()).toBe(false));

    expect(staleSignals).toHaveLength(1);
    expect(staleSignals.every((signal) => signal.aborted)).toBe(true);
    expect(listJobs).toHaveBeenCalledTimes(2);
    expect(store.getError()).toBeNull();
  });

  it("keeps an accepted close bound to its submitted job when a queued comment settles after navigation", async () => {
    const commentResponse = Promise.withResolvers<{
      id: number;
      created_at: string;
      responder: string;
      response: string;
      job_id: number;
    }>();
    let job42Closed = false;
    const closeBodies: Array<{ job_id: number; closed: boolean }> = [];
    const getReview = vi.fn((query: { job_id?: number }) => {
      const jobID = query.job_id ?? 42;
      return Promise.resolve({
        id: jobID,
        job_id: jobID,
        agent: "codex",
        created_at: "2026-08-04T12:00:00Z",
        output: `review ${jobID}`,
        prompt: "review",
        closed: jobID === 42 ? job42Closed : true,
      });
    });
    const getReviewProjection = vi.fn().mockResolvedValue({ responses: [] });
    const listJobs = vi.fn().mockResolvedValue({
      jobs: [],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });
    const addComment = vi.fn(() => commentResponse.promise);
    const closeReview = vi.fn(
      (request: { job_id: number; closed?: boolean }) => {
        const body = {
          job_id: request.job_id,
          closed: request.closed ?? false,
        };
        closeBodies.push(body);
        job42Closed = body.closed;
        return Promise.resolve({ success: true });
      },
    );
    const store = reviewStore({
      addComment,
      closeReview,
      getReview,
      getReviewProjection,
      listJobs,
    } as never);

    store.setSelectedJobId(42);
    store.loadReview(42);
    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(42));
    store.addComment(42, "hold the mutation queue");
    await vi.waitFor(() => expect(addComment).toHaveBeenCalledTimes(1));
    store.closeReview(42);

    store.setSelectedJobId(43);
    store.loadReview(43);
    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(43));
    commentResponse.resolve({
      id: 1,
      created_at: "2026-08-04T12:01:00Z",
      responder: "web",
      response: "hold the mutation queue",
      job_id: 42,
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
    const getReview = vi.fn().mockResolvedValue({
      id: 42,
      job_id: 42,
      agent: "codex",
      created_at: "2026-08-04T12:00:00Z",
      output: "review",
      prompt: "review",
      closed: false,
    });
    const getReviewProjection = vi.fn(() =>
      Promise.resolve({ responses: commentCommitted ? [acceptedComment] : [] }),
    );
    const listJobs = vi.fn().mockResolvedValue({
      jobs: [],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });
    const addComment = vi.fn().mockImplementation(() => {
      commentCommitted = true;
      return Promise.reject(new TypeError("response lost"));
    });
    const store = reviewStore({
      addComment,
      getReview,
      getReviewProjection,
      listJobs,
    } as never);
    const onSuccess = vi.fn();
    const onFailure = vi.fn();

    store.setSelectedJobId(42);
    store.loadReview(42);
    await vi.waitFor(() => expect(store.getReview()?.job_id).toBe(42));
    store.addComment(42, "retain this comment", { onSuccess, onFailure });

    await vi.waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
    expect(onFailure).not.toHaveBeenCalled();
    expect(addComment).toHaveBeenCalledTimes(1);
    expect(store.getResponses().map((response) => response.id)).toEqual([9]);
  });
});
