import type { RoborevClient } from "../api/client";
import type { SessionCapabilities } from "../api/session";
import type { OwnedAppRuntime } from "../runtime/runtime";
import { createDaemonStore, type DaemonStore } from "./roborev/daemon.svelte";
import { createJobsStore, type JobsStore } from "./roborev/jobs.svelte";
import { createLogStore, type LogStore } from "./roborev/log.svelte";
import { createReviewStore, type ReviewStore } from "./roborev/review.svelte";
import { makeRoborevOwner } from "./roborev/workflow";

export interface ReviewStores {
  roborevDaemon: DaemonStore;
  roborevJobs: JobsStore;
  roborevReview: ReviewStore;
  roborevLog: LogStore;
  getCapabilities: () => SessionCapabilities;
}

export interface ReviewStoreOptions {
  runtime: OwnedAppRuntime;
  client: RoborevClient;
  navigate: (jobId?: number) => void;
  getCapabilities: () => SessionCapabilities;
  onError?: (message: string) => void;
}

export function createReviewStores(options: ReviewStoreOptions): ReviewStores {
  const owner = makeRoborevOwner("native-reviews");
  const shared = {
    runtime: options.runtime,
    ...(options.onError !== undefined && { onError: options.onError }),
  };

  const roborevDaemon = createDaemonStore({
    client: options.client,
    runtime: options.runtime,
  });
  const roborevJobs = createJobsStore({
    client: options.client,
    owner,
    navigate: options.navigate,
    ...shared,
  });
  const roborevReview = createReviewStore({
    client: options.client,
    owner,
    refreshJobs: roborevJobs.loadJobsEffect,
    ...shared,
  });
  const roborevLog = createLogStore({
    baseUrl: "/",
    ...shared,
  });

  return {
    roborevDaemon,
    roborevJobs,
    roborevReview,
    roborevLog,
    getCapabilities: options.getCapabilities,
  };
}
