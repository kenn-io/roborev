import { execFile } from "node:child_process";
import { constants } from "node:fs";
import { access, mkdir, readFile, rename, rm } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

export interface GenerateAPIOptions {
  spec: string;
  output: string;
  check: boolean;
}

export type GeneratorRunner = (spec: string, output: string) => Promise<void>;

const runOpenAPITypeScript: GeneratorRunner = async (spec, output) => {
  const executable = resolve(
    import.meta.dirname,
    "../node_modules/.bin/openapi-typescript",
  );
  await access(executable, constants.X_OK);
  await execFileAsync(executable, [spec, "--output", output]);
};

export async function generateAPI(
  options: GenerateAPIOptions,
  runner: GeneratorRunner = runOpenAPITypeScript,
): Promise<void> {
  await mkdir(dirname(options.output), { recursive: true });
  const temporary = `${options.output}.tmp-${process.pid}-${crypto.randomUUID()}`;

  try {
    await runner(options.spec, temporary);
    if (options.check) {
      const [current, generated] = await Promise.all([
        readFile(options.output),
        readFile(temporary),
      ]);
      if (!current.equals(generated)) {
        throw new Error("generated browser API types are stale");
      }
      return;
    }
    await rename(temporary, options.output);
  } finally {
    await rm(temporary, { force: true });
  }
}

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  if (args.some((arg) => arg !== "--check") || args.length > 1) {
    console.error("usage: generate-api.ts [--check]");
    process.exitCode = 2;
    return;
  }
  const spec = resolve(import.meta.dirname, "../../pkg/client/openapi.yaml");
  const check = args[0] === "--check";
  await Promise.all([
    generateAPI({
      spec,
      output: resolve(import.meta.dirname, "../src/lib/api/generated.ts"),
      check,
    }),
    generateAPI({
      spec,
      output: resolve(
        import.meta.dirname,
        "../../packages/roborev-ui/src/generated.ts",
      ),
      check,
    }),
  ]);
}

if (import.meta.main) {
  await main();
}
