import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ReviewJob } from "../../api/generated/models";

vi.mock("./ReviewContent.svelte", async () => ({
  default: (await import("./ReviewDrawerTestContent.svelte")).default,
}));
vi.mock("./ResponseList.svelte", async () => ({
  default: (await import("./ReviewDrawerTestContent.svelte")).default,
}));
vi.mock("./LogViewer.svelte", async () => ({
  default: (await import("./ReviewDrawerTestContent.svelte")).default,
}));
vi.mock("./PromptViewer.svelte", async () => ({
  default: (await import("./ReviewDrawerTestContent.svelte")).default,
}));

const job: ReviewJob = {
  id: 42,
  agent: "claude",
  agentic: false,
  enqueued_at: "2026-07-15T00:00:00Z",
  git_ref: "abcdef123456",
  job_type: "review",
  prompt_prebuilt: false,
  repo_id: 1,
  repo_name: "example/repo",
  retry_count: 0,
  status: "queued",
};

// An off-page deep link: the job is absent from the table, so the drawer's
// job comes only from the review store's load.
const state = $state({
  job: null as ReviewJob | null,
  loading: true,
  loadError: null as string | null,
});
const reviewLoads = vi.hoisted(() => vi.fn());

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevJobs: {
      getVisibleJobs: () => [],
      getSelectedJobId: () => 42,
      deselectJob: vi.fn(),
      isRerunning: () => false,
      getPanelMemberError: () => undefined,
      isLoadingMembers: () => false,
      getPanelMembers: () => undefined,
      setPanelMemberInterest: vi.fn(),
    },
    roborevReview: {
      getSelectedJob: () => state.job,
      getSelectedJobId: () => 42,
      getReview: () => null,
      isLoading: () => state.loading,
      getError: () => state.loadError,
      isClosed: () => false,
      loadReview: reviewLoads,
    },
    getCapabilities: () => ({
      cancelAnyJob: true,
      cancelReviewJob: true,
      rerunJob: true,
    }),
  }),
}));

import ReviewDrawer from "./ReviewDrawer.svelte";

describe("ReviewDrawer log tab", () => {
  beforeEach(() => {
    state.job = null;
    state.loading = true;
    state.loadError = null;
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {}
      },
    );
    vi.stubGlobal(
      "MutationObserver",
      class {
        observe(): void {}
        disconnect(): void {}
      },
    );
    vi.stubGlobal("requestAnimationFrame", () => 1);
    vi.stubGlobal("cancelAnimationFrame", () => undefined);
  });

  afterEach(() => {
    cleanup();
    reviewLoads.mockReset();
    vi.unstubAllGlobals();
  });

  it("offers a retry when the detail load fails while Log is open", async () => {
    render(ReviewDrawer, { activeTab: "log" });

    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByTestId("review-drawer-content")).toBeNull();

    state.loading = false;
    state.loadError = "Failed to load review";
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Failed to load review",
    );

    reviewLoads.mockImplementation(() => {
      state.loadError = null;
      state.loading = true;
    });
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(reviewLoads).toHaveBeenCalledExactlyOnceWith(42);

    state.loading = false;
    state.job = job;
    expect(
      await screen.findByTestId("review-drawer-content"),
    ).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps a known job's log when the detail load fails", () => {
    state.job = job;
    state.loading = false;
    state.loadError = "Failed to load review";

    render(ReviewDrawer, { activeTab: "log" });

    expect(screen.getByTestId("review-drawer-content")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
