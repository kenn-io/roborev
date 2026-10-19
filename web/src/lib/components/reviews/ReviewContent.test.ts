import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import ReviewContent from "./ReviewContent.svelte";

const state = vi.hoisted(() => ({
  output: "## Review output",
  loading: false,
  notFound: false,
  status: "done",
  error: "",
}));
const errorReports = vi.hoisted(() => vi.fn());

vi.mock("../../utils/app-opened", () => ({
  reportErrorShown: errorReports,
}));

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevReview: {
      getOutput: () => state.output,
      isLoading: () => state.loading,
      isReviewNotFound: () => state.notFound,
      getSelectedJob: () => ({ status: state.status, error: state.error }),
    },
  }),
}));

vi.mock("@kenn-io/roborev-ui/review-content", () => {
  throw new Error("chunk unavailable");
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  errorReports.mockReset();
  state.output = "## Review output";
  state.loading = false;
  state.notFound = false;
  state.status = "done";
  state.error = "";
});

describe("ReviewContent", () => {
  it("offers a retry when the rich renderer cannot load", async () => {
    render(ReviewContent);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Could not load the review renderer.",
    );
    expect(errorReports).toHaveBeenCalledExactlyOnceWith("review_viewed");
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    await vi.waitFor(() => expect(errorReports).toHaveBeenCalledTimes(2));
    expect(errorReports).toHaveBeenLastCalledWith("review_viewed");
  });

  it("keeps a renderer failure without output silent", async () => {
    vi.useFakeTimers();
    state.output = "";
    render(ReviewContent);
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(errorReports).not.toHaveBeenCalled();
  });

  it("shows why a failed review produced no output", () => {
    state.output = "";
    state.notFound = true;
    state.status = "failed";
    state.error = "agent process exited with status 1";

    render(ReviewContent);

    expect(screen.getByRole("alert")).toHaveTextContent("Review failed");
    expect(screen.getByRole("alert")).toHaveTextContent(
      "agent process exited with status 1",
    );
    expect(screen.queryByText("Review in progress…")).toBeNull();
    expect(errorReports).not.toHaveBeenCalled();
  });

  it("explains a failed review even when no reason was recorded", () => {
    state.output = "";
    state.notFound = true;
    state.status = "failed";

    render(ReviewContent);

    expect(screen.getByRole("alert")).toHaveTextContent("Review failed");
    expect(screen.getByRole("alert")).toHaveTextContent(
      "The review agent failed before producing output.",
    );
  });
});
