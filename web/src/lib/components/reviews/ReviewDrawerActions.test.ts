import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { Effect } from "effect";
import { tick } from "svelte";
import { afterEach, expect, it, vi } from "vitest";
import { createReviewStores } from "../../stores/composition.svelte";
import { REVIEW_STORES_KEY } from "../../stores/context";
import { makeTestAppRuntime } from "../../testing/runtime";
import ReviewDrawer from "./ReviewDrawer.svelte";

const { reportCoreAction, reportErrorShown } = vi.hoisted(() => ({
  reportCoreAction: vi.fn(),
  reportErrorShown: vi.fn(),
}));
vi.mock("../../utils/app-opened", () => ({
  reportCoreAction,
  reportErrorShown,
}));

const runtime = makeTestAppRuntime();
const stores = createReviewStores({
  runtime,
  navigate: vi.fn(),
  getCapabilities: () => ({
    cancelAnyJob: true,
    cancelReviewJob: true,
    rerunJob: true,
  }),
});
vi.mock("@kenn-io/roborev-ui/review-content", () => {
  throw new Error("chunk unavailable");
});

afterEach(async () => {
  cleanup();
  stores.roborevJobs.dispose();
  await Effect.runPromise(runtime.disposeEffect);
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it("reports rendered review content after Retry and ignores job-list refreshes", async () => {
  reportCoreAction.mockClear();
  vi.stubGlobal("fetch", async (request: Request) => {
    const path = new URL(request.url).pathname;
    if (path === "/api/jobs") {
      return Response.json({
        jobs: [
          {
            id: 42,
            agent: "test",
            agentic: false,
            enqueued_at: "2026-01-01T00:00:00Z",
            git_ref: "abcdef12",
            job_type: "review",
            prompt_prebuilt: false,
            repo_id: 1,
            retry_count: 0,
            status: "done",
            readable: true,
          },
        ],
        has_more: false,
      });
    }
    if (path === "/api/review") {
      return Response.json({
        id: 1,
        job_id: 42,
        agent: "test",
        created_at: "2026-01-01T00:00:00Z",
        output: "# Review findings",
        closed: false,
      });
    }
    if (path === "/api/ui/review-projection") {
      return Response.json({ responses: [] });
    }
    throw new Error(`Unexpected request: ${path}`);
  });
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.stubGlobal("requestAnimationFrame", () => 1);
  vi.stubGlobal("cancelAnimationFrame", () => undefined);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({
    width: 900,
    height: 400,
    x: 0,
    y: 0,
    top: 0,
    right: 900,
    bottom: 400,
    left: 0,
    toJSON: () => ({}),
  });
  stores.roborevJobs.setSelectedJobId(42);
  stores.roborevReview.setSelectedJobId(42);
  const load = (effect: ReturnType<typeof stores.roborevJobs.loadJobsEffect>) =>
    Effect.runPromise(
      runtime.runCommand(effect, {
        operation: "load review fixture",
        safeContext: {},
        onFailure: () => {},
      }).await,
    );
  await load(stores.roborevJobs.loadJobsEffect());
  await load(stores.roborevReview.loadReviewEffect(42));
  render(ReviewDrawer, { context: new Map([[REVIEW_STORES_KEY, stores]]) });

  expect(await screen.findByRole("alert")).toHaveTextContent(
    "Could not load the review renderer.",
  );
  // A review reload remounts the same failure; it is not shown anew.
  for (let reload = 0; reload < 2; reload++) {
    await load(stores.roborevReview.loadReviewEffect(42));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Could not load the review renderer.",
    );
  }
  expect(reportCoreAction).not.toHaveBeenCalled();
  expect(reportErrorShown).toHaveBeenCalledExactlyOnceWith("review_viewed");
  vi.doMock("@kenn-io/roborev-ui/review-content", (importOriginal) =>
    importOriginal(),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(
    await screen.findByRole("heading", { name: "Review findings" }),
  ).toBeVisible();
  await vi.waitFor(() => expect(reportCoreAction).toHaveBeenCalledTimes(1));
  expect(reportCoreAction).toHaveBeenCalledWith("review_viewed");

  for (let refresh = 0; refresh < 2; refresh++) {
    await load(stores.roborevJobs.loadJobsEffect());
    await tick();
  }
  expect(reportCoreAction).toHaveBeenCalledTimes(1);
});
