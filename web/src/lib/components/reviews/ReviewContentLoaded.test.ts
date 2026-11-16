import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import ReviewContent from "./ReviewContent.svelte";

const state = vi.hoisted(() => ({
  error: "provider rejected the request",
  loadError: null as string | null,
  output: "",
  status: "failed",
  notFound: true,
  loaded: Promise.withResolvers<void>(),
}));
const reviewLoads = vi.hoisted(() => vi.fn());

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevReview: {
      getOutput: () => state.output,
      isLoading: () => false,
      isReviewNotFound: () => state.notFound,
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
  state.loadError = null;
  state.output = "";
  state.status = "failed";
  state.notFound = true;
});

beforeEach(() => {
  vi.stubGlobal(
    "requestIdleCallback",
    (callback: IdleRequestCallback): number => {
      callback({ didTimeout: false, timeRemaining: () => 50 });
      return 1;
    },
  );
  vi.stubGlobal("cancelIdleCallback", vi.fn());
});

it("keeps the failure reason visible after the rich renderer loads", async () => {
  render(ReviewContent);

  await state.loaded.promise;
  await tick();
  expect(screen.getByRole("alert")).toHaveTextContent("Review failed");
  expect(screen.getByRole("alert")).toHaveTextContent(state.error);
});

it("shows the load error after the renderer loads", async () => {
  state.loadError = "Failed to load review";
  state.output = "Existing review output";
  state.status = "done";
  state.notFound = false;
  render(ReviewContent);

  await vi.dynamicImportSettled();
  await tick();
  expect(screen.getByRole("alert")).toHaveTextContent("Failed to load review");
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(reviewLoads).toHaveBeenCalledExactlyOnceWith(42);
});
