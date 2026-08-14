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
import { makeAppRuntime, type OwnedAppRuntime } from "../../runtime/runtime";

import FilterBarTestHarness from "./FilterBarTestHarness.svelte";

type JobsStoreStub = {
  getFilterSearch: () => string | undefined;
  getFilterStatus: () => string | undefined;
  getFilterHideClosed: () => boolean;
  getFilterShowAutoDesign: () => boolean;
  getFilterRepo: () => string | undefined;
  getFilterBranch: () => string | undefined;
  setFilter: Mock<(key: string, value: string | boolean | undefined) => void>;
};

const state = {
  showAutoDesign: false,
  jobs: null as JobsStoreStub | null,
};

const client = {
  GET: vi.fn(),
};
let runtime: OwnedAppRuntime;

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevJobs: state.jobs,
  }),
  getRoborevClient: () => client,
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
      setFilter: vi.fn((key: string, value: string | boolean | undefined) => {
        if (key === "showAutoDesign") state.showAutoDesign = value === true;
      }),
    };
    client.GET.mockResolvedValue({ data: { repos: [] }, error: undefined });
  });

  afterEach(async () => {
    cleanup();
    state.jobs = null;
    client.GET.mockReset();
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
});
