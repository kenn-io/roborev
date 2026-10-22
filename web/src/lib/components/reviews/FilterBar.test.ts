import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { Effect } from "effect";
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
  type Mock,
} from "vitest";
import {
  makeAppRuntime,
  type AppExecution,
  type OwnedAppRuntime,
} from "../../runtime/runtime";

import FilterBarTestHarness from "./FilterBarTestHarness.svelte";

type JobsStoreStub = {
  getFilterSearch: () => string | undefined;
  getFilterStatus: () => string | undefined;
  getFilterHideClosed: () => boolean;
  getFilterShowAutoDesign: () => boolean;
  getFilterRepo: () => string | undefined;
  getFilterBranch: () => string | undefined;
  setFilter: Mock<(key: string, value: string | boolean | undefined) => void>;
  submitSearch: Mock<(value: string) => AppExecution<void, never> | undefined>;
};

const state = {
  showAutoDesign: false,
  jobs: null as JobsStoreStub | null,
};

const repoApi = vi.hoisted(() => ({
  listBranches: vi.fn(),
  listRepos: vi.fn(),
}));
let runtime: OwnedAppRuntime;

vi.mock("../../api/generated/repos/repos", () => repoApi);

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevJobs: state.jobs,
  }),
}));

describe("FilterBar", () => {
  beforeEach(() => {
    runtime = makeAppRuntime();
    state.showAutoDesign = false;
    state.jobs = {
      getFilterSearch: () => undefined,
      getFilterStatus: () => undefined,
      getFilterHideClosed: () => false,
      getFilterShowAutoDesign: () => state.showAutoDesign,
      getFilterRepo: () => undefined,
      getFilterBranch: () => undefined,
      submitSearch: vi.fn(),
      setFilter: vi.fn((key: string, value: string | boolean | undefined) => {
        if (key === "showAutoDesign") state.showAutoDesign = value === true;
      }),
    };
    repoApi.listRepos.mockResolvedValue({ repos: [] });
  });

  afterEach(async () => {
    cleanup();
    state.jobs = null;
    repoApi.listBranches.mockReset();
    repoApi.listRepos.mockReset();
    vi.useRealTimers();
    await Effect.runPromise(runtime.disposeEffect);
  });

  it("shows an unchecked auto-design toggle that enables the filter", async () => {
    render(FilterBarTestHarness, { props: { runtime } });

    const checkbox = screen.getByLabelText(
      "Show auto-design",
    ) as HTMLInputElement;
    expect(checkbox.checked).toBe(false);

    await fireEvent.click(checkbox);

    expect(state.jobs?.setFilter).toHaveBeenCalledWith("showAutoDesign", true);
  });

  it("does not apply a pending search after the filter bar unmounts", async () => {
    vi.useFakeTimers();
    const rendered = render(FilterBarTestHarness, { props: { runtime } });

    await fireEvent.input(
      screen.getByRole("searchbox", { name: "Search by ref" }),
      {
        target: { value: "feature/refactor" },
      },
    );
    rendered.unmount();
    await vi.advanceTimersByTimeAsync(300);

    expect(state.jobs?.setFilter).not.toHaveBeenCalledWith(
      "search",
      "feature/refactor",
    );
  });

  it("submits Enter with the current value and cancels the preview", async () => {
    vi.useFakeTimers();
    render(FilterBarTestHarness, { props: { runtime } });
    const input = screen.getByRole("searchbox", { name: "Search by ref" });
    await fireEvent.input(input, { target: { value: "feature/search" } });
    await fireEvent.keyDown(input, { key: "Enter", isComposing: true });
    expect(state.jobs?.submitSearch).not.toHaveBeenCalled();
    await fireEvent.keyDown(input, { key: "Enter", repeat: true });
    expect(state.jobs?.submitSearch).not.toHaveBeenCalled();
    await fireEvent.keyDown(input, { key: "Enter" });
    expect(state.jobs?.submitSearch).toHaveBeenCalledExactlyOnceWith(
      "feature/search",
    );
    await vi.advanceTimersByTimeAsync(300);
    expect(state.jobs?.setFilter).not.toHaveBeenCalled();
  });

  it("interrupts a submitted search as soon as typing resumes", async () => {
    vi.useFakeTimers();
    const reportEmpty = vi.fn();
    state.jobs!.submitSearch.mockImplementation(() =>
      runtime.runCommand(
        Effect.sleep("100 millis").pipe(
          Effect.andThen(Effect.sync(reportEmpty)),
        ),
        {
          operation: "test submitted search",
          safeContext: {},
          onFailure: () => {},
        },
      ),
    );
    render(FilterBarTestHarness, { props: { runtime } });
    const input = screen.getByRole("searchbox", { name: "Search by ref" });
    await fireEvent.input(input, { target: { value: "search-a" } });
    await fireEvent.keyDown(input, { key: "Enter" });
    await fireEvent.input(input, { target: { value: "search-b" } });
    await vi.advanceTimersByTimeAsync(100);

    expect(reportEmpty).not.toHaveBeenCalled();
    expect(state.jobs?.setFilter).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(200);
    expect(state.jobs?.setFilter).toHaveBeenCalledExactlyOnceWith(
      "search",
      "search-b",
    );
  });
});
