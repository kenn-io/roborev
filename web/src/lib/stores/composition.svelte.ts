import type { RoborevClient } from "../api/client";
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
}

export interface ReviewStoreOptions {
  runtime: OwnedAppRuntime;
  client: RoborevClient;
  navigate: (jobId?: number) => void;
  onError?: (message: string) => void;
}

export function createReviewStores(options: ReviewStoreOptions): ReviewStores {
  const owner = makeRoborevOwner("native-reviews");
  const shared = {
    runtime: options.runtime,
    ...(options.onError !== undefined && { onError: options.onError }),
  };

  return {
    roborevDaemon: createDaemonStore({
      client: options.client,
      runtime: options.runtime,
    }),
    roborevJobs: createJobsStore({
      client: options.client,
      owner,
      navigate: options.navigate,
      ...shared,
    }),
    roborevReview: createReviewStore({
      client: options.client,
      owner,
      ...shared,
    }),
    roborevLog: createLogStore({
      baseUrl: "/",
      ...shared,
    }),
  };
}
