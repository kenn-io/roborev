import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { components } from "../../api/generated";

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

const state = vi.hoisted(() => ({
  selectedJobId: 42 as number | undefined,
  deselectJob: vi.fn(),
  rerunJob: vi.fn(),
  cancelJob: vi.fn(),
  closeReview: vi.fn(),
  copyOutput: vi.fn(),
  jobStatus: "done",
  jobType: "review",
  agentic: false,
  promptPrebuilt: false,
  panelRole: undefined as string | undefined,
  rerunning: false,
  capabilities: {
    cancelAnyJob: true,
    cancelReviewJob: true,
    rerunJob: true,
  },
}));

type ReviewJob = components["schemas"]["ReviewJob"];
const job: ReviewJob = {
  id: 42,
  agent: "claude",
  agentic: false,
  enqueued_at: "2026-07-15T00:00:00Z",
  finished_at: "2026-07-15T00:01:00Z",
  git_ref: "abcdef123456",
  job_type: "review",
  prompt_prebuilt: false,
  repo_id: 1,
  repo_name: "example/repo",
  retry_count: 0,
  started_at: "2026-07-15T00:00:10Z",
  status: "done",
  token_usage: JSON.stringify({
    input_tokens: 231582,
    cached_input_tokens: 189952,
    total_output_tokens: 2542,
    peak_context_tokens: 47248,
    cost_usd: 0.347212,
    has_cost: true,
  }),
};

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevJobs: {
      getVisibleJobs: () => [
        {
          ...job,
          status: state.jobStatus,
          job_type: state.jobType,
          agentic: state.agentic,
          prompt_prebuilt: state.promptPrebuilt,
          panel_role: state.panelRole,
        },
      ],
      getSelectedJobId: () => state.selectedJobId,
      deselectJob: state.deselectJob,
      rerunJob: state.rerunJob,
      isRerunning: () => state.rerunning,
      cancelJob: state.cancelJob,
      getPanelMemberError: () => undefined,
      isLoadingMembers: () => false,
      getPanelMembers: () => undefined,
      setPanelMemberInterest: vi.fn(),
      refreshPanelMembers: vi.fn(),
    },
    roborevReview: {
      getSelectedJob: () => null,
      getOutput: () => "review output",
      getReview: () => ({
        id: 1,
        job_id: 42,
        output: "review output",
        closed: false,
      }),
      isClosed: () => false,
      closeReview: state.closeReview,
      copyOutput: state.copyOutput,
    },
    getCapabilities: () => state.capabilities,
  }),
}));

import ReviewDrawer from "./ReviewDrawer.svelte";

describe("ReviewDrawer", () => {
  beforeEach(() => {
    state.selectedJobId = 42;
    state.deselectJob.mockReset();
    state.rerunJob.mockReset();
    state.cancelJob.mockReset();
    state.closeReview.mockReset();
    state.copyOutput.mockReset();
    state.jobStatus = "done";
    state.jobType = "review";
    state.agentic = false;
    state.promptPrebuilt = false;
    state.panelRole = undefined;
    state.rerunning = false;
    state.capabilities = {
      cancelAnyJob: true,
      cancelReviewJob: true,
      rerunJob: true,
    };
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
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("renders the shared bottom dock with stable header, footer, and close control", async () => {
    render(ReviewDrawer);

    const dock = screen.getByRole("region", { name: "Review details" });
    expect(dock.classList.contains("kit-bottom-dock")).toBe(true);
    expect(dock.querySelector(".review-dock-header")).toBeTruthy();
    expect(dock.querySelector(".review-dock-footer")).toBeTruthy();

    await fireEvent.click(
      screen.getByRole("button", { name: "Close review details" }),
    );
    expect(state.deselectJob).toHaveBeenCalledTimes(1);
  });

  it("summarizes token usage instead of rendering the raw JSON blob", () => {
    render(ReviewDrawer);

    const usage = document.querySelector(".token-usage");
    expect(usage).toBeTruthy();
    expect(usage?.textContent).not.toContain("input_tokens");
    expect(
      [...(usage?.querySelectorAll(".usage-stat") ?? [])].map((el) =>
        el.textContent?.replace(/\s+/g, " ").trim(),
      ),
    ).toEqual(["cost ~$0.35", "in 232k", "out 2.5k", "peak 47k"]);
    expect(usage?.getAttribute("title")).toBe(
      "input 231,582 · cached input 189,952 · output 2,542 · peak context 47,248 · cost $0.347212",
    );
  });

  it("groups the footer actions as sibling shared buttons", () => {
    render(ReviewDrawer);

    const group = screen.getByRole("group", { name: "Review actions" });
    const buttons = [...group.children] as HTMLElement[];
    expect(buttons.map((el) => el.tagName)).toEqual([
      "BUTTON",
      "BUTTON",
      "BUTTON",
    ]);
    expect(buttons.every((el) => el.classList.contains("kit-button"))).toBe(
      true,
    );
    expect(buttons.map((el) => el.textContent?.trim())).toEqual([
      "Close Review",
      "Rerun",
      "Copy Output",
    ]);
  });

  it("keeps review tabs and actions application-owned", async () => {
    render(ReviewDrawer);

    await fireEvent.click(screen.getByRole("button", { name: "Log" }));
    expect(screen.getByTestId("review-drawer-content")).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Rerun" }));
    expect(state.rerunJob).toHaveBeenCalledWith(42);

    await fireEvent.click(screen.getByRole("button", { name: "Close Review" }));
    expect(state.closeReview).toHaveBeenCalledWith(42);

    await fireEvent.click(screen.getByRole("button", { name: "Copy Output" }));
    expect(state.copyOutput).toHaveBeenCalledTimes(1);
  });

  it("shows rerun only for rerunnable jobs", () => {
    state.jobStatus = "running";
    const running = render(ReviewDrawer);
    expect(screen.queryByRole("button", { name: "Rerun" })).toBeNull();
    running.unmount();

    state.jobStatus = "done";
    state.panelRole = "member";
    render(ReviewDrawer);
    expect(screen.queryByRole("button", { name: "Rerun" })).toBeNull();
  });

  it("disables rerun while the selected job is pending", () => {
    state.rerunning = true;
    render(ReviewDrawer);

    expect(screen.getByRole("button", { name: /Rerun/ })).toBeDisabled();
  });

  it("hides controls forbidden to token-authenticated sessions", () => {
    state.capabilities = {
      cancelAnyJob: false,
      cancelReviewJob: true,
      rerunJob: false,
    };
    const completedReview = render(ReviewDrawer);
    expect(screen.queryByRole("button", { name: "Rerun" })).toBeNull();
    completedReview.unmount();

    state.jobStatus = "queued";
    state.jobType = "compact";
    render(ReviewDrawer);
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
  });
});
