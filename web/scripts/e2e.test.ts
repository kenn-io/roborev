import { EventEmitter } from "node:events";
import { join } from "node:path";

import { describe, expect, test, vi } from "vitest";

import { installCleanupSignalHandlers, isolatedDaemonEnvironment } from "./e2e";

describe("browser test runner", () => {
  test("isolates daemon state and clears browser test routing", () => {
    const environment = isolatedDaemonEnvironment(
      {
        HOME: "/developer/home",
        USERPROFILE: "C:\\developer\\home",
        ROBOREV_DATA_DIR: "/developer/data",
        ROBOREV_WEB_DEV_BACKEND: "http://127.0.0.1:7373",
        ROBOREV_E2E_ORIGIN: "https://unexpected.example",
        ROBOREV_E2E_CONTROL_ORIGIN: "http://127.0.0.1:9000",
        ROBOREV_E2E_TOKEN: "unexpected-token",
        ROBOREV_TELEMETRY_ENABLED: "true",
        PATH: "/test/bin",
      },
      "/scratch/data",
      "/scratch/home",
    );

    expect(environment).toMatchObject({
      HOME: "/scratch/home",
      USERPROFILE: "/scratch/home",
      XDG_CACHE_HOME: join("/scratch/home", ".cache"),
      XDG_CONFIG_HOME: join("/scratch/home", ".config"),
      ROBOREV_DATA_DIR: "/scratch/data",
      ROBOREV_TELEMETRY_ENABLED: "0",
      PATH: "/test/bin",
    });
    expect(environment.ROBOREV_WEB_DEV_BACKEND).toBeUndefined();
    expect(environment.ROBOREV_E2E_ORIGIN).toBeUndefined();
    expect(environment.ROBOREV_E2E_CONTROL_ORIGIN).toBeUndefined();
    expect(environment.ROBOREV_E2E_TOKEN).toBeUndefined();
  });

  test.each([
    ["SIGINT", 130],
    ["SIGTERM", 143],
  ] as const)("cleans up once after %s", async (signal, exitCode) => {
    const target = new EventEmitter() as EventEmitter & { exitCode?: number };
    const cleanup = vi.fn(async () => {});
    const remove = installCleanupSignalHandlers(cleanup, target);

    target.emit(signal);
    target.emit(signal);

    await vi.waitFor(() => expect(cleanup).toHaveBeenCalledTimes(1));
    expect(target.exitCode).toBe(exitCode);
    expect(target.listenerCount("SIGINT")).toBe(1);
    expect(target.listenerCount("SIGTERM")).toBe(1);
    remove();
    expect(target.listenerCount("SIGINT")).toBe(0);
    expect(target.listenerCount("SIGTERM")).toBe(0);
  });
});
