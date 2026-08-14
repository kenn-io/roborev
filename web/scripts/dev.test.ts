import { describe, expect, test } from "vitest";

import {
  runWebDev,
  type ChildProcessHandle,
  type DevDependencies,
  type SpawnRequest,
} from "./dev";

class FakeChild implements ChildProcessHandle {
  readonly signals: NodeJS.Signals[] = [];
  private resolveExit!: (code: number | null) => void;
  readonly exited = new Promise<number | null>((resolve) => {
    this.resolveExit = resolve;
  });

  finish(code: number | null): void {
    this.resolveExit(code);
  }

  kill(signal: NodeJS.Signals): void {
    this.signals.push(signal);
    this.finish(null);
  }
}

describe("full-stack web development", () => {
  test("isolates the daemon, discovers its listener, and cleans up", async () => {
    const root = "/tmp/roborev-web-test";
    const daemon = new FakeChild();
    const vite = new FakeChild();
    const spawns: SpawnRequest[] = [];
    let removed = "";
    let signalHandler: ((signal: NodeJS.Signals) => void) | undefined;
    const dependencies: DevDependencies = {
      cwd: "/work/roborev/web",
      env: {
        PATH: "/usr/bin",
        ROBOREV_DATA_DIR: "/real/data",
      },
      async makeTempRoot() {
        return root;
      },
      async prepareRoot() {},
      async allocatePort() {
        return 43123;
      },
      spawn(request) {
        spawns.push(request);
        return spawns.length === 1 ? daemon : vite;
      },
      async waitForRuntime(dataDir, expectedOrigin) {
        expect(dataDir).toBe(`${root}/data`);
        expect(expectedOrigin).toBe("http://127.0.0.1:43123");
        return {
          address: "127.0.0.1:7373",
          webAddress: "127.0.0.1:44001",
          webOrigin: expectedOrigin,
        };
      },
      registerSignals(handler) {
        signalHandler = handler;
        return () => {
          signalHandler = undefined;
        };
      },
      async removeTempRoot(path) {
        removed = path;
      },
    };

    const running = runWebDev(dependencies);
    await viWaitFor(() => spawns.length === 2);
    signalHandler?.("SIGTERM");
    await expect(running).resolves.toBe(0);

    const daemonRequest = spawns[0];
    expect(daemonRequest.command).toBe("go");
    expect(daemonRequest.args).toEqual([
      "run",
      "./cmd/roborev",
      "daemon",
      "run",
      "--db",
      `${root}/reviews.db`,
      "--config",
      `${root}/config.toml`,
      "--addr",
      "127.0.0.1:0",
      "--web-dev-origin",
      "http://127.0.0.1:43123",
    ]);
    expect(daemonRequest.cwd).toBe("/work/roborev");
    expect(daemonRequest.env.ROBOREV_DATA_DIR).toBe(`${root}/data`);
    expect(daemonRequest.env.ROBOREV_DATA_DIR).not.toBe("/real/data");

    const viteRequest = spawns[1];
    expect(viteRequest).toMatchObject({
      command: "bun",
      args: ["run", "dev", "--", "--port", "43123"],
      cwd: "/work/roborev/web",
    });
    expect(viteRequest.env.ROBOREV_WEB_DEV_BACKEND).toBe(
      "http://127.0.0.1:44001",
    );
    expect(daemon.signals).toContain("SIGTERM");
    expect(vite.signals).toContain("SIGTERM");
    expect(removed).toBe(root);
  });

  test("does not spawn children when termination arrives during preparation", async () => {
    const spawns: SpawnRequest[] = [];
    let signalHandler: ((signal: NodeJS.Signals) => void) | undefined;
    let removed = false;
    const dependencies: DevDependencies = {
      cwd: "/work/roborev/web",
      env: {},
      async makeTempRoot() {
        return "/tmp/roborev-web-canceled";
      },
      async prepareRoot() {
        signalHandler?.("SIGTERM");
      },
      async allocatePort() {
        return 43123;
      },
      spawn(request) {
        spawns.push(request);
        return new FakeChild();
      },
      async waitForRuntime() {
        throw new Error("must not wait for runtime after cancellation");
      },
      registerSignals(handler) {
        signalHandler = handler;
        return () => {
          signalHandler = undefined;
        };
      },
      async removeTempRoot() {
        removed = true;
      },
    };

    await expect(runWebDev(dependencies)).resolves.toBe(0);
    expect(spawns).toEqual([]);
    expect(removed).toBe(true);
  });
});

async function viWaitFor(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 1_000;
  while (!predicate()) {
    if (Date.now() > deadline) {
      throw new Error("condition was not reached");
    }
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
}
