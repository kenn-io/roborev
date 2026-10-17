import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import { SvelteMap } from "svelte/reactivity";
import { afterEach, describe, expect, it, vi } from "vitest";

import ReviewContent from "./ReviewContent.svelte";

const state = vi.hoisted(() => ({
  output: "## Review output",
  loading: false,
  notFound: false,
  status: "done",
  error: "",
  report: vi.fn(),
}));

const reactiveJob = new SvelteMap<
  number,
  { id: number; status: string; started_at: string; error: string }
>();

vi.mock("../../utils/app-opened", () => ({ reportErrorShown: state.report }));

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevReview: {
      getOutput: () => state.output,
      isLoading: () => state.loading,
      isReviewNotFound: () => state.notFound,
      getSelectedJob: () =>
        reactiveJob.get(1) ?? {
          id: 1,
          status: state.status,
          error: state.error,
        },
    },
  }),
}));

vi.mock("@kenn-io/roborev-ui/review-content", () => {
  throw new Error("chunk unavailable");
});

afterEach(() => {
  cleanup();
  reactiveJob.clear();
  state.output = "## Review output";
  state.loading = false;
  state.notFound = false;
  state.status = "done";
  state.error = "";
  state.report.mockClear();
  vi.restoreAllMocks();
});

describe("ReviewContent", () => {
  it("counts a failed retry once and clears the previous visible failure", async () => {
    state.output = "";
    const job = {
      id: 1,
      status: "failed",
      started_at: "2026-01-01T00:00:00Z",
      error: "agent failed",
    };
    reactiveJob.set(1, job);
    render(ReviewContent);
    expect(state.report).toHaveBeenCalledTimes(1);
    reactiveJob.set(1, { ...job });
    await tick();
    expect(state.report).toHaveBeenCalledTimes(1);
    reactiveJob.set(1, { ...job, status: "running" });
    await tick();
    reactiveJob.set(1, { ...job, started_at: "2026-01-01T00:01:00Z" });
    await tick();
    expect(state.report).toHaveBeenCalledTimes(2);
    reactiveJob.set(1, { ...job, started_at: "2026-01-01T00:02:00Z" });
    await tick();
    expect(state.report).toHaveBeenCalledTimes(3);
  });
  it("defers failed reviews until the tab becomes visible", async () => {
    let visibility = "hidden";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(
      () => visibility as DocumentVisibilityState,
    );
    state.status = "failed";
    state.output = "";
    render(ReviewContent);
    expect(state.report).not.toHaveBeenCalled();
    visibility = "visible";
    await fireEvent(document, new Event("visibilitychange"));
    expect(state.report).toHaveBeenCalledExactlyOnceWith("review_viewed");
    await fireEvent(document, new Event("visibilitychange"));
    expect(state.report).toHaveBeenCalledTimes(1);
  });
  it("offers a retry when the rich renderer cannot load", async () => {
    render(ReviewContent);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Could not load the review renderer.",
    );
    expect(state.report).toHaveBeenCalledExactlyOnceWith("review_viewed");
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    await vi.waitFor(() => expect(state.report).toHaveBeenCalledTimes(2));
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
    expect(state.report).toHaveBeenCalledExactlyOnceWith("review_viewed");
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
