import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/svelte";
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

import RepoTreePicker from "./RepoTreePicker.svelte";

type JobsStoreStub = {
  getFilterRepo: () => string | undefined;
  getFilterBranch: () => string | undefined;
  setFilter: Mock<(key: string, value: string | undefined) => void>;
};

const state = {
  repo: undefined as string | undefined,
  branch: undefined as string | undefined,
  jobs: null as JobsStoreStub | null,
  runtime: null as OwnedAppRuntime | null,
};

const repoApi = vi.hoisted(() => ({
  listBranches: vi.fn(),
  listRepos: vi.fn(),
}));

vi.mock("../../api/generated/repos/repos", () => repoApi);

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevJobs: state.jobs,
  }),
}));

vi.mock("../../runtime/context", () => ({
  getAppRuntime: () => {
    if (state.runtime === null)
      throw new Error("test runtime was not initialized");
    return state.runtime;
  },
}));

describe("RepoTreePicker", () => {
  beforeEach(() => {
    state.repo = undefined;
    state.branch = undefined;
    state.runtime = makeAppRuntime();
    state.jobs = {
      getFilterRepo: () => state.repo,
      getFilterBranch: () => state.branch,
      setFilter: vi.fn((key: string, value: string | undefined) => {
        if (key === "repo") state.repo = value;
        if (key === "branch") state.branch = value;
      }),
    };
    repoApi.listRepos.mockResolvedValue({
      repos: [
        {
          root_path: "/workspace/project-a",
          name: "project-a",
          count: 4,
        },
      ],
    });
  });

  afterEach(async () => {
    cleanup();
    state.jobs = null;
    repoApi.listBranches.mockReset();
    repoApi.listRepos.mockReset();
    if (state.runtime !== null)
      await Effect.runPromise(state.runtime.disposeEffect);
    state.runtime = null;
  });

  it("closes when pressing outside the picker", async () => {
    render(RepoTreePicker);

    await fireEvent.click(screen.getByRole("button", { name: /all repos/i }));
    expect(screen.getByPlaceholderText("Filter repos...")).toBeTruthy();

    await fireEvent.mouseDown(document.body);

    expect(screen.queryByPlaceholderText("Filter repos...")).toBeNull();
  });

  it("keeps the newer repository loading while an older branch request is canceled", async () => {
    const projectABranches = Promise.withResolvers<{
      branches: Array<{ name: string; count: number }>;
    }>();
    const projectBBranches = Promise.withResolvers<{
      branches: Array<{ name: string; count: number }>;
    }>();
    repoApi.listRepos.mockResolvedValue({
      repos: [
        {
          root_path: "/workspace/project-a",
          name: "project-a",
          count: 4,
        },
        {
          root_path: "/workspace/project-b",
          name: "project-b",
          count: 2,
        },
      ],
    });
    repoApi.listBranches.mockImplementation((query) => {
      const repo = query?.repo?.[0];
      return repo === "/workspace/project-a"
        ? projectABranches.promise
        : projectBBranches.promise;
    });
    render(RepoTreePicker);

    await fireEvent.click(screen.getByRole("button", { name: /all repos/i }));
    await screen.findByText("project-b");
    const expandButtons = screen.getAllByTitle("Show branches");

    await fireEvent.click(expandButtons[0]);
    await waitFor(() =>
      expect(repoApi.listBranches).toHaveBeenCalledWith(
        { repo: ["/workspace/project-a"] },
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
    await fireEvent.click(expandButtons[1]);
    await waitFor(() =>
      expect(repoApi.listBranches).toHaveBeenCalledWith(
        { repo: ["/workspace/project-b"] },
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );

    expect(screen.getByText("Loading...")).toBeTruthy();
    expect(screen.queryByText("No branches")).toBeNull();

    projectABranches.resolve({
      branches: [{ name: "stale-a", count: 1 }],
    });
    await Promise.resolve();
    expect(screen.getByText("Loading...")).toBeTruthy();
    expect(screen.queryByText("stale-a")).toBeNull();

    projectBBranches.resolve({
      branches: [{ name: "current-b", count: 1 }],
    });
    await screen.findByText("current-b");
    expect(screen.queryByText("stale-a")).toBeNull();
  });

  it("does not expose a previous repository's branches when the next request fails", async () => {
    repoApi.listRepos.mockResolvedValue({
      repos: [
        {
          root_path: "/workspace/project-a",
          name: "project-a",
          count: 4,
        },
        {
          root_path: "/workspace/project-b",
          name: "project-b",
          count: 2,
        },
      ],
    });
    repoApi.listBranches.mockImplementation((query) => {
      if (query?.repo?.[0] === "/workspace/project-a") {
        return Promise.resolve({
          branches: [{ name: "project-a-main", count: 1 }],
        });
      }
      return Promise.reject(new TypeError("branch lookup failed"));
    });
    render(RepoTreePicker);

    await fireEvent.click(screen.getByRole("button", { name: /all repos/i }));
    await screen.findByText("project-b");
    const expandButtons = screen.getAllByTitle("Show branches");

    await fireEvent.click(expandButtons[0]);
    await screen.findByText("project-a-main");
    await fireEvent.click(expandButtons[1]);

    await screen.findByText("No branches");
    expect(screen.queryByText("project-a-main")).toBeNull();
  });
});
