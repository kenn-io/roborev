import { cleanup, render, screen } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import DaemonStatus from "./DaemonStatus.svelte";

type DaemonStoreStub = {
  isAvailable: () => boolean;
  getVersion: () => string;
  getActiveWorkers: () => number;
  getMaxWorkers: () => number;
  getQueuedJobs: () => number;
  getRunningJobs: () => number;
  getCompletedJobs: () => number;
  getFailedJobs: () => number;
  checkHealth: () => Promise<void>;
};

const state = {
  daemon: null as DaemonStoreStub | null,
  filteredCounts: undefined as
    | { queued: number; running: number; done: number; failed: number }
    | undefined,
  filteredScope: false,
};

vi.mock("../../stores/context", () => ({
  getReviewStores: () => ({
    roborevDaemon: state.daemon,
    roborevJobs: {
      getFilteredStatusCounts: () => state.filteredCounts,
      usesFilteredStatusCounts: () => state.filteredScope,
    },
  }),
}));

function createDaemonStore(version: string): DaemonStoreStub {
  return {
    isAvailable: () => true,
    getVersion: () => version,
    getActiveWorkers: () => 1,
    getMaxWorkers: () => 4,
    getQueuedJobs: () => 2,
    getRunningJobs: () => 1,
    getCompletedJobs: () => 5,
    getFailedJobs: () => 0,
    checkHealth: vi.fn(async () => undefined),
  };
}

describe("DaemonStatus", () => {
  beforeEach(() => {
    state.daemon = createDaemonStore("v0.52.0");
  });

  afterEach(() => {
    cleanup();
    state.daemon = null;
    state.filteredCounts = undefined;
    state.filteredScope = false;
  });

  it("does not prepend another v when the daemon version already has one", () => {
    render(DaemonStatus);

    expect(screen.getByTitle("Daemon version").textContent).toBe("v0.52.0");
  });

  it("prepends v when the daemon version is returned without one", () => {
    state.daemon = createDaemonStore("0.52.0");

    render(DaemonStatus);

    expect(screen.getByTitle("Daemon version").textContent).toBe("v0.52.0");
  });

  it("shows counts from the filtered jobs view when available", () => {
    state.filteredScope = true;
    state.filteredCounts = { queued: 1, running: 0, done: 3, failed: 2 };

    render(DaemonStatus);

    expect(screen.getByTitle("Queued").textContent?.trim()).toBe("1 queued");
    expect(screen.getByTitle("Running").textContent?.trim()).toBe("0 running");
    expect(screen.getByTitle("Done").textContent?.trim()).toBe("3 done");
    expect(screen.getByTitle("Failed").textContent?.trim()).toBe("2 failed");
  });

  it("does not fall back to daemon totals before scoped counts are available", () => {
    state.filteredScope = true;
    state.daemon = {
      ...createDaemonStore("v0.52.0"),
      getFailedJobs: () => 9,
    };

    render(DaemonStatus);

    expect(screen.getByTitle("Failed").textContent?.trim()).toBe("-- failed");
  });
});
