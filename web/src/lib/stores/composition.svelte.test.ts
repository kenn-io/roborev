import { Effect } from "effect";
import { afterEach, describe, expect, test, vi } from "vitest";

import { makeAppRuntime, type OwnedAppRuntime } from "../runtime/runtime";
import { createReviewStores } from "./composition.svelte";

let runtime: OwnedAppRuntime | undefined;

afterEach(async () => {
  if (runtime !== undefined) {
    await Effect.runPromise(runtime.disposeEffect);
  }
  runtime = undefined;
});

describe("review store composition", () => {
  test("routes job selection through the native navigation adapter", () => {
    runtime = makeAppRuntime();
    const navigate = vi.fn();
    const stores = createReviewStores({
      runtime,
      client: {} as never,
      navigate,
      getCapabilities: () => ({
        cancelAnyJob: true,
        cancelReviewJob: true,
        rerunJob: true,
      }),
    });

    stores.roborevJobs.selectJob(42);
    stores.roborevJobs.deselectJob();

    expect(navigate.mock.calls).toEqual([[42], []]);
    stores.roborevJobs.dispose();
  });

  test("refreshes jobs after closing a review without an event stream", async () => {
    runtime = makeAppRuntime();
    let closed = false;
    let listRequests = 0;
    const get = vi.fn(
      (
        path: string,
        options?: { params?: { query?: { id?: number; job_id?: number } } },
      ) => {
        if (path === "/api/review") {
          return Promise.resolve({
            data: {
              id: 7,
              job_id: 42,
              agent: "codex",
              created_at: "2026-08-14T12:00:00Z",
              output: "review",
              closed,
            },
            error: undefined,
          });
        }
        if (path === "/api/ui/review-projection") {
          return Promise.resolve({ data: { responses: [] }, error: undefined });
        }
        if (options?.params?.query?.id === 42) {
          return Promise.resolve({
            data: { jobs: [], has_more: false },
            error: undefined,
          });
        }
        listRequests += 1;
        return Promise.resolve({
          data: {
            jobs: [
              {
                id: 42,
                agent: "codex",
                agentic: false,
                enqueued_at: "2026-08-14T12:00:00Z",
                git_ref: "deadbeef",
                job_type: "review",
                prompt_prebuilt: false,
                repo_id: 1,
                retry_count: 0,
                status: "done",
                closed,
              },
            ],
            has_more: false,
            stats: {
              queued: 0,
              running: 0,
              done: 1,
              failed: 0,
              canceled: 0,
              skipped: 0,
              closed: closed ? 1 : 0,
              open: closed ? 0 : 1,
            },
          },
          error: undefined,
        });
      },
    );
    const post = vi.fn(
      (_path: string, options: { body: { closed: boolean } }) => {
        closed = options.body.closed;
        return Promise.resolve({ data: { success: true }, error: undefined });
      },
    );
    const stores = createReviewStores({
      runtime,
      client: { GET: get, POST: post } as never,
      navigate: vi.fn(),
      getCapabilities: () => ({
        cancelAnyJob: true,
        cancelReviewJob: true,
        rerunJob: true,
      }),
    });

    expect(stores.roborevJobs.isEventStreamConnected()).toBe(false);
    stores.roborevReview.setSelectedJobId(42);
    stores.roborevReview.loadReview(42);
    await vi.waitFor(() =>
      expect(stores.roborevReview.getReview()?.job_id).toBe(42),
    );

    stores.roborevReview.closeReview(42);

    await vi.waitFor(() => expect(listRequests).toBe(1));
    expect(stores.roborevJobs.getJobs()[0]?.closed).toBe(true);
    expect(stores.roborevJobs.getStats().closed).toBe(1);
    stores.roborevJobs.dispose();
  });
});
