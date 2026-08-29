import { cleanup, render, screen } from "@testing-library/svelte";
import { afterEach, expect, it, vi } from "vitest";

import ReviewContent from "./ReviewContent.svelte";

const state = vi.hoisted(() => ({
  error: "provider rejected the request",
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

afterEach(cleanup);

it("keeps the failure reason visible after the rich renderer loads", async () => {
  await import("@kenn-io/roborev-ui/review-content");
  render(ReviewContent);

  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(screen.getByRole("alert")).toHaveTextContent("Review failed");
  expect(screen.getByRole("alert")).toHaveTextContent(state.error);
});
