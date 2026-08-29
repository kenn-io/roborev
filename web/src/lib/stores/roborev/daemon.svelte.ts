import { Duration, Effect, Option, Schedule } from "effect";

import { TransientTransportError } from "../../api/effect-errors";
import type { RoborevClient } from "../../api/client";
import type { components } from "../../api/generated";
import type { AppRuntime } from "../../runtime/runtime";

const UNAVAILABLE_POLL_INTERVAL_MS = 1_000;
const AVAILABLE_POLL_INTERVAL_MS = 30_000;
const STATUS_TIMEOUT = "5 seconds";

type DaemonStatus = components["schemas"]["DaemonStatus"];

export interface DaemonStoreOptions {
  client: RoborevClient;
  runtime: AppRuntime;
  initiallyAvailable?: boolean;
}

export function createDaemonStore(opts: DaemonStoreOptions) {
  let available = $state(opts.initiallyAvailable ?? false);
  let wasEverAvailable = $state(false);
  let version = $state("");
  let endpoint = $state("");
  let loading = $state(false);
  let queuedJobs = $state(0);
  let runningJobs = $state(0);
  let completedJobs = $state(0);
  let failedJobs = $state(0);
  let canceledJobs = $state(0);
  let activeWorkers = $state(0);
  let maxWorkers = $state(0);
  let statusGeneration = 0;

  function clearStatus(): void {
    queuedJobs = 0;
    runningJobs = 0;
    completedJobs = 0;
    failedJobs = 0;
    canceledJobs = 0;
    activeWorkers = 0;
    maxWorkers = 0;
  }

  function applyStatus(status: DaemonStatus): void {
    queuedJobs = status.queued_jobs;
    runningJobs = status.running_jobs;
    completedJobs = status.completed_jobs;
    failedJobs = status.failed_jobs;
    canceledJobs = status.canceled_jobs;
    activeWorkers = status.active_workers;
    maxWorkers = status.max_workers;
    if (status.version) version = status.version;
  }

  const readStatus = Effect.tryPromise({
    try: (signal) => opts.client.GET("/api/status", { signal }),
    catch: (cause) =>
      TransientTransportError.make({
        operation: "GET Roborev daemon status",
        cause,
      }),
  }).pipe(
    Effect.flatMap((result) =>
      result.data === undefined
        ? Effect.fail(
            TransientTransportError.make({
              operation: "GET Roborev daemon status",
              cause:
                result.error ?? new Error("Roborev status response was empty"),
            }),
          )
        : Effect.succeed(result.data),
    ),
    Effect.timeout(STATUS_TIMEOUT),
  );

  const healthProgram = Effect.gen(function* () {
    const requestGeneration = yield* Effect.sync(() => ++statusGeneration);
    yield* Effect.sync(() => {
      loading = true;
    });
    const result = yield* readStatus.pipe(Effect.option);
    if (requestGeneration !== statusGeneration) return false;
    yield* Effect.sync(() => {
      loading = false;
    });
    if (Option.isNone(result)) {
      yield* Effect.sync(() => {
        available = false;
        clearStatus();
      });
      return false;
    }

    const recovered = !available;
    yield* Effect.sync(() => {
      available = true;
      wasEverAvailable = true;
      endpoint = globalThis.location.origin;
      applyStatus(result.value);
    });
    return recovered;
  });

  const pollingSchedule = Schedule.forever.pipe(
    Schedule.addDelay(() =>
      Effect.succeed(
        Duration.millis(
          available ? AVAILABLE_POLL_INTERVAL_MS : UNAVAILABLE_POLL_INTERVAL_MS,
        ),
      ),
    ),
  );
  const pollingEffect = healthProgram.pipe(Effect.repeat(pollingSchedule));

  function runStatus(operation: string): void {
    opts.runtime.runCommand(healthProgram, {
      operation,
      safeContext: {},
      onFailure: () => {},
    });
  }

  return {
    isAvailable: () => available,
    getVersion: () => version,
    getEndpoint: () => endpoint,
    isLoading: () => loading,
    getQueuedJobs: () => queuedJobs,
    getRunningJobs: () => runningJobs,
    getCompletedJobs: () => completedJobs,
    getFailedJobs: () => failedJobs,
    getCanceledJobs: () => canceledJobs,
    getActiveWorkers: () => activeWorkers,
    getMaxWorkers: () => maxWorkers,
    getWasEverAvailable: () => wasEverAvailable,
    checkHealth: () => runStatus("check Roborev daemon health"),
    loadStatus: () => runStatus("load Roborev daemon status"),
    pollingEffect,
  };
}

export type DaemonStore = ReturnType<typeof createDaemonStore>;
