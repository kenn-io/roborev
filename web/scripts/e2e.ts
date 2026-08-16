import { spawn, type ChildProcess } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createServer as createHTTPServer } from "node:http";
import { type AddressInfo, type Server } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { compilationStub } from "./embed-assets";

interface RuntimeRecord {
  metadata?: Record<string, string>;
}

interface SignalEmitter {
  exitCode?: string | number | null;
  on(signal: "SIGINT" | "SIGTERM", listener: () => void): unknown;
  removeListener(signal: "SIGINT" | "SIGTERM", listener: () => void): unknown;
}

const browserToken = "MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1ub3BxcnN0dXY";

export async function runBrowserTests(): Promise<number> {
  const webRoot = resolve(import.meta.dirname, "..");
  const repoRoot = dirname(webRoot);
  const scratch = await mkdtemp(join(tmpdir(), "roborev-web-e2e-"));
  const dataDir = join(scratch, "data");
  const homeDir = join(scratch, "home");
  const database = join(scratch, "reviews.db");
  const jobLogDir = join(dataDir, "logs", "jobs");
  const config = join(scratch, "config.toml");
  const binary = join(
    scratch,
    process.platform === "win32" ? "roborev.exe" : "roborev",
  );
  let assetsEmbedded = false;
  let daemon: ChildProcess | undefined;
  let controlServer: Server | undefined;
  let activeRestart: Promise<void> | undefined;
  const commands = new Set<ChildProcess>();
  let cleanupPromise: Promise<void> | undefined;
  let terminating = false;
  const assertRunning = (): void => {
    if (terminating) throw new Error("browser test run was interrupted");
  };
  const cleanup = (): Promise<void> => {
    cleanupPromise ??= (async () => {
      terminating = true;
      if (controlServer) {
        await closeServer(controlServer);
      }
      if (activeRestart) await activeRestart.catch(() => undefined);
      await Promise.all(Array.from(commands, stop));
      if (daemon) {
        await stop(daemon);
      }
      if (assetsEmbedded) {
        await run("bun", ["run", "assets:restore"], webRoot);
        assetsEmbedded = false;
      }
      await rm(scratch, { recursive: true, force: true });
    })();
    return cleanupPromise;
  };
  const removeSignalHandlers = installCleanupSignalHandlers(cleanup);

  try {
    const sourceIndex = await readFile(
      join(repoRoot, "internal", "web", "dist", "index.html"),
      "utf8",
    );
    assertRunning();
    if (sourceIndex !== compilationStub) {
      throw new Error("embedded web assets must start at the compilation stub");
    }

    await Promise.all([
      mkdir(dataDir, { recursive: true, mode: 0o700 }),
      mkdir(homeDir, { recursive: true, mode: 0o700 }),
    ]);
    assertRunning();
    await writeFile(config, browserConfig(0), { mode: 0o600 });
    assertRunning();

    assertRunning();
    await run(
      "go",
      ["run", "./internal/testutil/cmd/seed-web", "-out", database],
      repoRoot,
      {},
      true,
      commands,
    );
    assertRunning();
    await mkdir(jobLogDir, { recursive: true, mode: 0o700 });
    await writeFile(
      join(jobLogDir, "52.log"),
      "fixture review started\nstreamed analysis complete\n",
      { mode: 0o600 },
    );
    assertRunning();
    assertRunning();
    await run("bun", ["run", "build"], webRoot, {}, true, commands);
    assertRunning();
    assetsEmbedded = true;
    assertRunning();
    await run("bun", ["run", "assets:embed"], webRoot, {}, true, commands);
    assertRunning();
    assertRunning();
    await run(
      "go",
      ["build", "-o", binary, "./cmd/roborev"],
      repoRoot,
      {},
      true,
      commands,
    );
    assertRunning();
    assertRunning();
    await run("bun", ["run", "assets:restore"], webRoot, {}, true, commands);
    assertRunning();
    assetsEmbedded = false;

    const startDaemon = (): ChildProcess => {
      assertRunning();
      return spawn(
        binary,
        [
          "daemon",
          "run",
          "--db",
          database,
          "--config",
          config,
          "--addr",
          "127.0.0.1:0",
        ],
        {
          cwd: repoRoot,
          env: isolatedDaemonEnvironment(process.env, dataDir, homeDir),
          stdio: "inherit",
          detached: process.platform !== "win32",
        },
      );
    };
    daemon = startDaemon();
    const origin = await waitForBrowserOrigin(dataDir, daemon);
    assertRunning();
    await writeFile(config, browserConfig(Number(new URL(origin).port)), {
      mode: 0o600,
    });
    assertRunning();
    controlServer = createHTTPServer((request, response) => {
      if (request.method !== "POST" || request.url !== "/restart") {
        response.writeHead(404).end();
        return;
      }
      activeRestart = (async () => {
        assertRunning();
        if (daemon) await stop(daemon);
        assertRunning();
        daemon = startDaemon();
        const restartedOrigin = await waitForBrowserOrigin(dataDir, daemon);
        assertRunning();
        if (restartedOrigin !== origin) {
          throw new Error("browser origin changed after daemon restart");
        }
        response.writeHead(204).end();
      })();
      void activeRestart.catch((error: unknown) => {
        response
          .writeHead(500, { "Content-Type": "text/plain" })
          .end(error instanceof Error ? error.message : String(error));
      });
    });
    const controlOrigin = await listenLoopback(controlServer);
    assertRunning();
    const remoteResult = await run(
      "bunx",
      [
        "playwright",
        "test",
        "--config",
        "playwright.config.ts",
        "--grep-invert",
        "@local-session",
      ],
      webRoot,
      {
        ROBOREV_E2E_CONTROL_ORIGIN: controlOrigin,
        ROBOREV_E2E_ORIGIN: origin,
        ROBOREV_E2E_TOKEN: browserToken,
      },
      false,
      commands,
    );
    if (remoteResult !== 0) return remoteResult;

    if (daemon) await stop(daemon);
    assertRunning();
    await writeFile(
      config,
      browserConfig(Number(new URL(origin).port), false),
      { mode: 0o600 },
    );
    daemon = startDaemon();
    const localOrigin = await waitForBrowserOrigin(dataDir, daemon);
    if (localOrigin !== origin) {
      throw new Error("browser origin changed for local-session tests");
    }

    return await run(
      "bunx",
      [
        "playwright",
        "test",
        "--config",
        "playwright.config.ts",
        "--grep",
        "@local-session",
      ],
      webRoot,
      {
        ROBOREV_E2E_CONTROL_ORIGIN: controlOrigin,
        ROBOREV_E2E_ORIGIN: localOrigin,
        ROBOREV_E2E_TOKEN: "",
      },
      false,
      commands,
    );
  } finally {
    await cleanup();
    removeSignalHandlers();
  }
}

export function installCleanupSignalHandlers(
  cleanup: () => Promise<void>,
  target: SignalEmitter = process,
): () => void {
  let handlingSignal = false;
  const handle = (exitCode: number) => () => {
    if (handlingSignal) return;
    handlingSignal = true;
    target.exitCode = exitCode;
    void cleanup();
  };
  const interrupt = handle(130);
  const terminate = handle(143);
  target.on("SIGINT", interrupt);
  target.on("SIGTERM", terminate);
  return () => {
    target.removeListener("SIGINT", interrupt);
    target.removeListener("SIGTERM", terminate);
  };
}

export function isolatedDaemonEnvironment(
  source: NodeJS.ProcessEnv,
  dataDir: string,
  homeDir: string,
): NodeJS.ProcessEnv {
  const environment: NodeJS.ProcessEnv = {
    ...source,
    HOME: homeDir,
    USERPROFILE: homeDir,
    XDG_CACHE_HOME: join(homeDir, ".cache"),
    XDG_CONFIG_HOME: join(homeDir, ".config"),
    ROBOREV_DATA_DIR: dataDir,
    ROBOREV_TELEMETRY_ENABLED: "0",
  };
  delete environment.ROBOREV_WEB_DEV_BACKEND;
  delete environment.ROBOREV_E2E_ORIGIN;
  delete environment.ROBOREV_E2E_CONTROL_ORIGIN;
  delete environment.ROBOREV_E2E_TOKEN;
  return environment;
}

function browserConfig(port: number, tokenAuthentication = true): string {
  const auth = tokenAuthentication ? `auth_token = "${browserToken}"\n` : "";
  return `max_workers = 0\n\n[web]\nenabled = true\nlisten = "127.0.0.1:${port}"\n${auth}`;
}

async function listenLoopback(server: Server): Promise<string> {
  await new Promise<void>((resolveListen, rejectListen) => {
    server.once("error", rejectListen);
    server.listen(0, "127.0.0.1", () => {
      server.removeListener("error", rejectListen);
      resolveListen();
    });
  });
  const address = server.address() as AddressInfo | null;
  if (address === null) throw new Error("loopback test server did not bind");
  return `http://127.0.0.1:${address.port}`;
}

async function closeServer(server: Server): Promise<void> {
  if (!server.listening) return;
  await new Promise<void>((resolveClose, rejectClose) => {
    server.close((error) => (error ? rejectClose(error) : resolveClose()));
  });
}

async function run(
  command: string,
  args: string[],
  cwd: string,
  extraEnvironment: Record<string, string> = {},
  rejectOnFailure = true,
  children?: Set<ChildProcess>,
): Promise<number> {
  const child = spawn(command, args, {
    cwd,
    env: { ...process.env, ...extraEnvironment },
    stdio: "inherit",
    detached: process.platform !== "win32",
  });
  children?.add(child);
  const code = await childExit(child).finally(() => children?.delete(child));
  if (rejectOnFailure && code !== 0) {
    throw new Error(`${command} exited with status ${code}`);
  }
  return code;
}

async function waitForBrowserOrigin(
  dataDir: string,
  daemon: ChildProcess,
): Promise<string> {
  if (!daemon.pid) {
    throw new Error("browser test daemon did not start");
  }
  const runtime = join(dataDir, "runtime", `daemon.${daemon.pid}.json`);
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    if (daemon.exitCode !== null || daemon.signalCode !== null) {
      throw new Error(
        "browser test daemon exited before publishing runtime metadata",
      );
    }
    try {
      const record = JSON.parse(
        await readFile(runtime, "utf8"),
      ) as RuntimeRecord;
      const origin = record.metadata?.web_origin;
      if (origin) {
        const ping = await fetch(`${origin}/api/ping`);
        if (ping.ok) return origin;
      }
    } catch {
      // Runtime publication and listener startup are both asynchronous.
    }
    await delay(25);
  }
  throw new Error("timed out waiting for the browser test daemon");
}

async function stop(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  signalProcessTree(child, "SIGTERM");
  if (await exitsWithin(child, 3_000)) return;
  signalProcessTree(child, "SIGKILL");
  await exitsWithin(child, 1_000);
}

function signalProcessTree(child: ChildProcess, signal: NodeJS.Signals): void {
  if (!child.pid) return;
  if (process.platform === "win32") {
    const killer = spawn("taskkill", ["/PID", String(child.pid), "/T", "/F"], {
      stdio: "ignore",
    });
    killer.unref();
    return;
  }
  try {
    process.kill(-child.pid, signal);
  } catch {
    // The process group may have exited between the state check and signal.
  }
}

function exitsWithin(
  child: ChildProcess,
  milliseconds: number,
): Promise<boolean> {
  return Promise.race([
    childExit(child).then(() => true),
    delay(milliseconds).then(() => false),
  ]);
}

function childExit(child: ChildProcess): Promise<number> {
  return new Promise((resolveExit) => {
    if (child.exitCode !== null) {
      resolveExit(child.exitCode);
      return;
    }
    child.once("error", () => resolveExit(1));
    child.once("exit", (code) => resolveExit(code ?? 1));
  });
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, milliseconds));
}

const entrypoint = process.argv[1];
if (entrypoint && resolve(entrypoint) === fileURLToPath(import.meta.url)) {
  process.exitCode = await runBrowserTests();
}
