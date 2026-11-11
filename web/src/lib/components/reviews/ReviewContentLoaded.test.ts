import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import { afterEach, expect, it, vi } from "vitest";

import ReviewContent from "./ReviewContent.svelte";

const reviewLoads = vi.hoisted(() => vi.fn());
const state = vi.hoisted(() => ({
  error: "provider rejected the request",
  loadError: null as string | null,
  output: "",
  status: "failed",
  loaded: Promise.withResolvers<void>(),
}));

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevReview: {
      getOutput: () => state.output,
      isLoading: () => false,
      isReviewNotFound: () => true,
      getSelectedJob: () => ({ status: state.status, error: state.error }),
      getSelectedJobId: () => 42,
      getError: () => state.loadError,
      loadReview: reviewLoads,
    },
  }),
}));

vi.mock("@kenn-io/roborev-ui/review-content", () => {
  state.loaded.resolve();
  return { default: () => {} };
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  reviewLoads.mockReset();
  state.loadError = null;
  state.output = "";
  state.status = "failed";
});

function stubIdleCallback(): void {
  vi.stubGlobal(
    "requestIdleCallback",
    (callback: IdleRequestCallback): number => {
      callback({ didTimeout: false, timeRemaining: () => 50 });
      return 1;
    },
  );
  vi.stubGlobal("cancelIdleCallback", vi.fn());
}

it("keeps the failure reason visible after the rich renderer loads", async () => {
  stubIdleCallback();
  render(ReviewContent);

  await state.loaded.promise;
  await tick();
  expect(screen.getByRole("alert")).toHaveTextContent("Review failed");
  expect(screen.getByRole("alert")).toHaveTextContent(state.error);
});

it("shows a review load failure instead of the loaded renderer", async () => {
  stubIdleCallback();
  state.loadError = "Failed to load review";
  state.output = "Existing review output";
  state.status = "done";
  render(ReviewContent);

  await state.loaded.promise;
  await tick();
  expect(screen.getByRole("alert")).toHaveTextContent("Failed to load review");
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(reviewLoads).toHaveBeenCalledExactlyOnceWith(42);
});
