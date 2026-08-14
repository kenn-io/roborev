import { Effect, Exit } from "effect";
import { afterEach, describe, expect, test, vi } from "vitest";

import type { OwnedAppRuntime } from "./runtime";
import { makeAppRuntime } from "./runtime";

let runtime: OwnedAppRuntime | undefined;

describe("native app runtime", () => {
  afterEach(async () => {
    if (runtime !== undefined) {
      await Effect.runPromise(runtime.disposeEffect);
    }
    runtime = undefined;
  });

  test("exposes an owned result to Promise-only integrations", async () => {
    runtime = makeAppRuntime();
    const execution = runtime.runCommand(Effect.succeed("accepted"), {
      operation: "test Promise integration",
      safeContext: {},
      onFailure: () => {},
    });

    const exit = await execution.exit;

    expect(Exit.isSuccess(exit) && exit.value).toBe("accepted");
  });

  test("reports typed failures but ignores interruption-only exits", async () => {
    runtime = makeAppRuntime();
    const onFailure = vi.fn();
    const failed = runtime.runCommand(Effect.fail("typed failure"), {
      operation: "test typed failure",
      safeContext: {},
      onFailure,
    });
    await failed.exit;
    expect(onFailure).toHaveBeenCalledWith("typed failure");

    const interrupted = runtime.runCommand(Effect.never, {
      operation: "test interruption",
      safeContext: {},
      onFailure,
    });
    interrupted.interrupt();
    await interrupted.exit;
    expect(onFailure).toHaveBeenCalledTimes(1);
  });

  test("does not publish an interrupted microtask command", async () => {
    runtime = makeAppRuntime();
    const callback = vi.fn();
    const execution = runtime.runMicrotask(callback, {
      operation: "test deferred publication",
      safeContext: {},
    });

    execution.interrupt();
    await execution.exit;

    expect(callback).not.toHaveBeenCalled();
  });
});
