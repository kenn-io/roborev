import { cp, mkdir, rename, rm, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";

import { validateAssetGraph } from "./validate-assets";

export const compilationStub = `<!doctype html>
<html lang="en">
  <head><meta charset="utf-8"><title>Roborev</title></head>
  <body>Roborev web assets are not built.</body>
</html>
`;

export interface EmbedOperations {
  rename?: typeof rename;
}

const defaultTarget = resolve(import.meta.dirname, "../../internal/web/dist");

export async function embedAssets(
  source: string,
  target = defaultTarget,
  operations: EmbedOperations = {},
): Promise<void> {
  await validateAssetGraph(source);
  await replaceDirectory(
    target,
    async (staging) => {
      await cp(source, staging, { recursive: true, errorOnExist: true });
      await validateAssetGraph(staging);
    },
    operations,
  );
}

export async function restoreCompilationStub(
  target = defaultTarget,
): Promise<void> {
  await replaceDirectory(target, async (staging) => {
    await mkdir(staging, { recursive: true });
    await writeFile(`${staging}/index.html`, compilationStub, { mode: 0o644 });
  });
}

async function replaceDirectory(
  target: string,
  populate: (staging: string) => Promise<void>,
  operations: EmbedOperations = {},
): Promise<void> {
  const renamePath = operations.rename ?? rename;
  const parent = dirname(target);
  const suffix = `${process.pid}-${crypto.randomUUID()}`;
  const staging = `${parent}/.dist-staging-${suffix}`;
  const backup = `${parent}/.dist-backup-${suffix}`;
  let movedOriginal = false;

  await mkdir(parent, { recursive: true });
  try {
    await populate(staging);
    await renamePath(target, backup);
    movedOriginal = true;
    try {
      await renamePath(staging, target);
    } catch (error) {
      await renamePath(backup, target);
      movedOriginal = false;
      throw error;
    }
    movedOriginal = false;
    await rm(backup, { recursive: true, force: true });
  } finally {
    await rm(staging, { recursive: true, force: true });
    if (movedOriginal) {
      await renamePath(backup, target);
    }
    await rm(backup, { recursive: true, force: true });
  }
}

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  if (args.length === 1 && args[0] === "--restore-stub") {
    await restoreCompilationStub();
    return;
  }
  if (args.length === 1) {
    await embedAssets(resolve(args[0]!));
    return;
  }
  console.error("usage: embed-assets.ts <distribution> | --restore-stub");
  process.exitCode = 2;
}

if (import.meta.main) {
  await main();
}
