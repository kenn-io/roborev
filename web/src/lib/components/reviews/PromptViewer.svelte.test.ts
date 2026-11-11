import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import PromptViewer from "./PromptViewer.svelte";

const state = $state({
  prompt: "",
  loading: false,
  loadError: null as string | null,
});
const reviewLoads = vi.hoisted(() => vi.fn());

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevReview: {
      getPrompt: () => state.prompt,
      isLoading: () => state.loading,
      getError: () => state.loadError,
      getSelectedJobId: () => 42,
      loadReview: reviewLoads,
    },
  }),
}));

afterEach(() => {
  cleanup();
  reviewLoads.mockReset();
  state.prompt = "";
  state.loading = false;
  state.loadError = null;
});

describe("PromptViewer", () => {
  it("offers a retry for the selected job when switched to after a failed load", async () => {
    state.loadError = "Failed to load review";
    reviewLoads.mockImplementation(() => {
      state.loadError = null;
      state.loading = true;
    });

    render(PromptViewer);

    expect(screen.getByRole("alert")).toHaveTextContent(
      "Failed to load review",
    );
    expect(screen.queryByText("No prompt available.")).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(reviewLoads).toHaveBeenCalledExactlyOnceWith(42);
    expect(await screen.findByText("Loading prompt…")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps the empty state for a loaded job without a prompt", () => {
    render(PromptViewer);

    expect(screen.getByText("No prompt available.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("shows an available prompt even when the detail load failed", () => {
    state.prompt = "Prompt from the job row";
    state.loadError = "Failed to load review";

    render(PromptViewer);

    expect(screen.getByText("Prompt from the job row")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
