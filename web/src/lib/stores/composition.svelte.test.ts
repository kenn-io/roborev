import { Effect } from "effect";
import { afterEach, describe, expect, test, vi } from "vitest";

import { makeAppRuntime, type OwnedAppRuntime } from "../runtime/runtime";
import { createReviewStores } from "./composition.svelte";

let runtime: OwnedAppRuntime | undefined;

afterEach(async () => {
  if (runtime !== undefined) {
    await Effect.runPromise(runtime.disposeEffect);
  }
  runtime = undefined;
});

describe("review store composition", () => {
  test("routes job selection through the native navigation adapter", () => {
    runtime = makeAppRuntime();
    const navigate = vi.fn();
    const stores = createReviewStores({
      runtime,
      client: {} as never,
      navigate,
    });

    stores.roborevJobs.selectJob(42);
    stores.roborevJobs.deselectJob();

    expect(navigate.mock.calls).toEqual([[42], []]);
    stores.roborevJobs.dispose();
  });
});
