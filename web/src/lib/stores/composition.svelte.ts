import { appPath } from "../base-path";
import type { SessionCapabilities } from "../api/session";
import type { OwnedAppRuntime } from "../runtime/runtime";
import {
  createDaemonStore,
  type DaemonStore,
  type DaemonStoreOptions,
} from "./roborev/daemon.svelte";
import {
  createJobsStore,
  type JobsStore,
  type JobsStoreOptions,
} from "./roborev/jobs.svelte";
import { createLogStore, type LogStore } from "./roborev/log.svelte";
import {
  createReviewStore,
  type ReviewStore,
  type ReviewStoreOptions as NativeReviewStoreOptions,
} from "./roborev/review.svelte";
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
  daemonStatus?: DaemonStoreOptions["getStatus"];
  jobsApi?: JobsStoreOptions["api"];
  reviewApi?: NativeReviewStoreOptions["api"];
  navigate: (jobId?: number) => void;
  getCapabilities: () => SessionCapabilities;
  onError?: (message: string) => void;
  daemonInitiallyAvailable?: boolean;
}

export function createReviewStores(options: ReviewStoreOptions): ReviewStores {
  const owner = makeRoborevOwner("native-reviews");
  const shared = {
    runtime: options.runtime,
    ...(options.onError !== undefined && { onError: options.onError }),
  };

  const roborevDaemon = createDaemonStore({
    ...(options.daemonStatus !== undefined && {
      getStatus: options.daemonStatus,
    }),
    runtime: options.runtime,
    initiallyAvailable: options.daemonInitiallyAvailable,
  });
  const roborevJobs = createJobsStore({
    ...(options.jobsApi !== undefined && { api: options.jobsApi }),
    owner,
    navigate: options.navigate,
    ...shared,
  });
  const roborevReview = createReviewStore({
    ...(options.reviewApi !== undefined && { api: options.reviewApi }),
    owner,
    refreshJobs: roborevJobs.loadJobsEffect,
    ...shared,
  });
  const roborevLog = createLogStore({
    baseUrl: appPath("/"),
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
