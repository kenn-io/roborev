import { spawn as nodeSpawn } from "node:child_process";
import {
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export interface ChildProcessHandle {
  exited: Promise<number | null>;
  kill(signal: NodeJS.Signals): void;
}

export interface SpawnRequest {
  command: string;
  args: string[];
  cwd: string;
  env: Record<string, string | undefined>;
}

export interface BrowserRuntime {
  address: string;
  webAddress: string;
  webOrigin: string;
}

export interface DevDependencies {
  cwd: string;
  env: Record<string, string | undefined>;
  makeTempRoot(): Promise<string>;
  prepareRoot(root: string): Promise<void>;
  allocatePort(): Promise<number>;
  spawn(request: SpawnRequest): ChildProcessHandle;
  waitForRuntime(
    dataDir: string,
    expectedOrigin: string,
  ): Promise<BrowserRuntime>;
  registerSignals(handler: (signal: NodeJS.Signals) => void): () => void;
  removeTempRoot(root: string): Promise<void>;
}

export async function runWebDev(
  dependencies: DevDependencies = defaultDependencies(),
): Promise<number> {
  let root = "";
  const children: ChildProcessHandle[] = [];
  let terminatingSignal: NodeJS.Signals = "SIGTERM";
  let terminationRequested = false;
  const unregisterSignals = dependencies.registerSignals((signal) => {
    terminationRequested = true;
    terminatingSignal = signal;
    for (const child of children) {
      child.kill(signal);
    }
  });

  try {
    root = await dependencies.makeTempRoot();
    if (terminationRequested) return 0;
    await dependencies.prepareRoot(root);
    if (terminationRequested) return 0;
    const vitePort = await dependencies.allocatePort();
    if (terminationRequested) return 0;
    const viteOrigin = `http://127.0.0.1:${vitePort}`;
    const dataDir = join(root, "data");
    const webDir = resolve(dependencies.cwd);
    const repositoryRoot = dirname(webDir);
    const daemonEnvironment = {
      ...dependencies.env,
      ROBOREV_DATA_DIR: dataDir,
      ROBOREV_WEB_DEV_BACKEND: undefined,
    };
    const daemon = dependencies.spawn({
      command: "go",
      args: [
        "run",
        "./cmd/roborev",
        "daemon",
        "run",
        "--db",
        join(root, "reviews.db"),
        "--config",
        join(root, "config.toml"),
        "--addr",
        "127.0.0.1:0",
        "--web-dev-origin",
        viteOrigin,
      ],
      cwd: repositoryRoot,
      env: daemonEnvironment,
    });
    children.push(daemon);
    if (terminationRequested) return 0;

    const browserRuntime = await Promise.race([
      dependencies.waitForRuntime(dataDir, viteOrigin),
      daemon.exited.then((code) => {
        throw new Error(
          `development daemon exited before ready (${String(code)})`,
        );
      }),
    ]);
    if (terminationRequested) return 0;
    const vite = dependencies.spawn({
      command: "bun",
      args: ["run", "dev", "--", "--port", String(vitePort)],
      cwd: webDir,
      env: {
        ...dependencies.env,
        ROBOREV_DATA_DIR: undefined,
        ROBOREV_WEB_DEV_BACKEND: `http://${browserRuntime.webAddress}`,
      },
    });
    children.push(vite);
    if (terminationRequested) return 0;
    process.stdout.write(`Roborev web development server: ${viteOrigin}\n`);

    const code = await Promise.race(children.map((child) => child.exited));
    return code ?? 0;
  } finally {
    unregisterSignals();
    await Promise.all(
      children.map((child) => stopChild(child, terminatingSignal)),
    );
    if (root !== "") {
      await dependencies.removeTempRoot(root);
    }
  }
}

function defaultDependencies(): DevDependencies {
  return {
    cwd: process.cwd(),
    env: { ...process.env },
    makeTempRoot: () => mkdtemp(join(tmpdir(), "roborev-web-")),
    async prepareRoot(root) {
      await mkdir(join(root, "data"), { recursive: true, mode: 0o700 });
      await writeFile(
        join(root, "config.toml"),
        '[web]\nenabled = true\nlisten = "127.0.0.1:0"\n',
        { mode: 0o600 },
      );
    },
    allocatePort,
    spawn: spawnChild,
    waitForRuntime,
    registerSignals,
    removeTempRoot: (root) => rm(root, { recursive: true, force: true }),
  };
}

function spawnChild(request: SpawnRequest): ChildProcessHandle {
  const child = nodeSpawn(request.command, request.args, {
    cwd: request.cwd,
    detached: process.platform !== "win32",
    env: request.env,
    stdio: "inherit",
  });
  return {
    exited: new Promise((resolveExit) => {
      child.once("error", () => resolveExit(1));
      child.once("exit", (code) => resolveExit(code));
    }),
    kill(signal) {
      if (child.exitCode === null && child.signalCode === null) {
        if (process.platform !== "win32" && child.pid !== undefined) {
          try {
            process.kill(-child.pid, signal);
          } catch {
            // The process group may have exited between the state check and kill.
          }
        } else {
          child.kill(signal);
        }
      }
    },
  };
}

async function allocatePort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const server = createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      if (address === null || typeof address === "string") {
        server.close();
        reject(new Error("could not allocate a Vite development port"));
        return;
      }
      server.close((error) => {
        if (error) {
          reject(error);
        } else {
          resolvePort(address.port);
        }
      });
    });
  });
}

async function waitForRuntime(
  dataDir: string,
  expectedOrigin: string,
): Promise<BrowserRuntime> {
  const runtimeDir = join(dataDir, "runtime");
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    let names: string[] = [];
    try {
      names = await readdir(runtimeDir);
    } catch {
      await delay(25);
      continue;
    }
    for (const name of names) {
      if (!/^daemon\.\d+\.json$/.test(name)) {
        continue;
      }
      try {
        const raw = JSON.parse(
          await readFile(join(runtimeDir, name), "utf8"),
        ) as Record<string, unknown>;
        const metadata = raw.metadata as Record<string, unknown> | undefined;
        if (
          typeof raw.address === "string" &&
          typeof metadata?.web_address === "string" &&
          metadata.web_origin === expectedOrigin
        ) {
          return {
            address: raw.address,
            webAddress: metadata.web_address,
            webOrigin: expectedOrigin,
          };
        }
      } catch {
        // Atomic runtime publication can still race directory enumeration.
      }
    }
    await delay(25);
  }
  throw new Error("timed out waiting for the development daemon runtime");
}

function registerSignals(
  handler: (signal: NodeJS.Signals) => void,
): () => void {
  const signals: NodeJS.Signals[] = ["SIGINT", "SIGTERM"];
  const listeners = new Map<NodeJS.Signals, () => void>();
  for (const signal of signals) {
    const listener = () => handler(signal);
    listeners.set(signal, listener);
    process.once(signal, listener);
  }
  return () => {
    for (const [signal, listener] of listeners) {
      process.off(signal, listener);
    }
  };
}

async function stopChild(
  child: ChildProcessHandle,
  signal: NodeJS.Signals,
): Promise<void> {
  child.kill(signal);
  if (await exitsWithin(child, 2_000)) {
    return;
  }
  child.kill("SIGKILL");
  await exitsWithin(child, 1_000);
}

async function exitsWithin(
  child: ChildProcessHandle,
  timeout: number,
): Promise<boolean> {
  return Promise.race([
    child.exited.then(() => true),
    delay(timeout).then(() => false),
  ]);
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, milliseconds));
}

const entrypoint = process.argv[1];
if (entrypoint && resolve(entrypoint) === fileURLToPath(import.meta.url)) {
  const code = await runWebDev();
  process.exitCode = code;
}
