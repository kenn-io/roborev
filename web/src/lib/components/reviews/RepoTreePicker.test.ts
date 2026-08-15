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

const client = {
  GET: vi.fn(),
};

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevJobs: state.jobs,
  }),
  getRoborevClient: () => client,
}));

vi.mock("../../runtime/context", () => ({
  getRoborevClient: () => client,
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
    client.GET.mockResolvedValue({
      data: {
        repos: [
          {
            root_path: "/workspace/project-a",
            name: "project-a",
            count: 4,
          },
        ],
      },
    });
  });

  afterEach(async () => {
    cleanup();
    state.jobs = null;
    client.GET.mockReset();
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
      data: { branches: Array<{ name: string; count: number }> };
    }>();
    const projectBBranches = Promise.withResolvers<{
      data: { branches: Array<{ name: string; count: number }> };
    }>();
    client.GET.mockImplementation((path, options) => {
      if (path === "/api/repos") {
        return Promise.resolve({
          data: {
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
          },
        });
      }
      const repo = options?.params?.query?.repo?.[0];
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
      expect(client.GET).toHaveBeenCalledWith(
        "/api/branches",
        expect.objectContaining({
          params: { query: { repo: ["/workspace/project-a"] } },
        }),
      ),
    );
    await fireEvent.click(expandButtons[1]);
    await waitFor(() =>
      expect(client.GET).toHaveBeenCalledWith(
        "/api/branches",
        expect.objectContaining({
          params: { query: { repo: ["/workspace/project-b"] } },
        }),
      ),
    );

    expect(screen.getByText("Loading...")).toBeTruthy();
    expect(screen.queryByText("No branches")).toBeNull();

    projectABranches.resolve({
      data: { branches: [{ name: "stale-a", count: 1 }] },
    });
    await Promise.resolve();
    expect(screen.getByText("Loading...")).toBeTruthy();
    expect(screen.queryByText("stale-a")).toBeNull();

    projectBBranches.resolve({
      data: { branches: [{ name: "current-b", count: 1 }] },
    });
    await screen.findByText("current-b");
    expect(screen.queryByText("stale-a")).toBeNull();
  });

  it("does not expose a previous repository's branches when the next request fails", async () => {
    client.GET.mockImplementation((path, options) => {
      if (path === "/api/repos") {
        return Promise.resolve({
          data: {
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
          },
        });
      }
      const repo = options?.params?.query?.repo?.[0];
      return Promise.resolve(
        repo === "/workspace/project-a"
          ? { data: { branches: [{ name: "project-a-main", count: 1 }] } }
          : { error: { message: "branch lookup failed" } },
      );
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
