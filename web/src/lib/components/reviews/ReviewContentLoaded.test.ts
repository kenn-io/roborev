import { cleanup, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import { afterEach, expect, it, vi } from "vitest";

import ReviewContent from "./ReviewContent.svelte";

const state = vi.hoisted(() => ({
  error: "provider rejected the request",
  loaded: Promise.withResolvers<void>(),
}));

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevReview: {
      getOutput: () => "",
      isLoading: () => false,
      isReviewNotFound: () => true,
      getSelectedJob: () => ({ status: "failed", error: state.error }),
    },
  }),
}));

vi.mock("@kenn-io/roborev-ui/review-content", async (importOriginal) => {
  const module = await importOriginal();
  state.loaded.resolve();
  return module;
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it("keeps the failure reason visible after the rich renderer loads", async () => {
  vi.stubGlobal(
    "requestIdleCallback",
    (callback: IdleRequestCallback): number => {
      callback({ didTimeout: false, timeRemaining: () => 50 });
      return 1;
    },
  );
  vi.stubGlobal("cancelIdleCallback", vi.fn());
  render(ReviewContent);

  await state.loaded.promise;
  await tick();
  expect(screen.getByRole("alert")).toHaveTextContent("Review failed");
  expect(screen.getByRole("alert")).toHaveTextContent(state.error);
});
