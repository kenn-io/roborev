import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import ReviewContent from "./ReviewContent.svelte";

const state = vi.hoisted(() => ({
  output: "## Review output",
  loading: false,
  notFound: false,
  status: "done",
}));

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevReview: {
      getOutput: () => state.output,
      isLoading: () => state.loading,
      isReviewNotFound: () => state.notFound,
      getSelectedJob: () => ({ status: state.status }),
    },
  }),
}));

vi.mock("@kenn-io/roborev-ui/review-content", () => {
  throw new Error("chunk unavailable");
});

afterEach(() => {
  cleanup();
  state.output = "## Review output";
  state.loading = false;
  state.notFound = false;
  state.status = "done";
});

describe("ReviewContent", () => {
  it("offers a retry when the rich renderer cannot load", async () => {
    render(ReviewContent);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Could not load the review renderer.",
    );
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
  });

  it("does not call a terminal job with no review in progress", async () => {
    state.output = "";
    state.notFound = true;
    state.status = "failed";

    render(ReviewContent);

    expect(screen.getByText("No review output available.")).toBeInTheDocument();
    expect(screen.queryByText("Review in progress…")).toBeNull();
  });
});
