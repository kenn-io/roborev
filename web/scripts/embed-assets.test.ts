// @vitest-environment node

import {
  mkdtemp,
  mkdir,
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, expect, test } from "vitest";

import {
  compilationStub,
  embedAssets,
  restoreCompilationStub,
  type EmbedOperations,
} from "./embed-assets";

const temporaryDirectories: string[] = [];

afterEach(async () => {
  await Promise.all(
    temporaryDirectories
      .splice(0)
      .map((directory) => rm(directory, { recursive: true, force: true })),
  );
});

async function fixture(): Promise<{ source: string; target: string }> {
  const root = await mkdtemp(join(tmpdir(), "roborev-embed-"));
  temporaryDirectories.push(root);
  const source = join(root, "source");
  const target = join(root, "target");
  await mkdir(join(source, ".vite"), { recursive: true });
  await mkdir(join(source, "assets"), { recursive: true });
  await mkdir(target);
  await writeFile(
    join(source, "index.html"),
    '<meta name="roborev-web-distribution" content="production">',
  );
  await writeFile(
    join(source, ".vite/manifest.json"),
    JSON.stringify({ "index.html": { file: "assets/index-a1b2c3.js" } }),
  );
  await writeFile(join(source, "assets/index-a1b2c3.js"), "export {};\n");
  await writeFile(join(target, "index.html"), compilationStub);
  return { source, target };
}

test("stages a complete distribution and restores only the canonical stub", async () => {
  const { source, target } = await fixture();
  await embedAssets(source, target);
  await expect(
    readFile(join(target, "assets/index-a1b2c3.js"), "utf8"),
  ).resolves.toBe("export {};\n");

  await restoreCompilationStub(target);
  await expect(readdir(target)).resolves.toEqual(["index.html"]);
  await expect(readFile(join(target, "index.html"), "utf8")).resolves.toBe(
    compilationStub,
  );
});

test("restores the previous distribution when activation fails", async () => {
  const { source, target } = await fixture();
  const operations: EmbedOperations = {
    rename: async (from, to) => {
      if (String(to) === target && String(from).includes(".dist-staging-")) {
        throw new Error("injected activation failure");
      }
      await import("node:fs/promises").then((fs) => fs.rename(from, to));
    },
  };

  await expect(embedAssets(source, target, operations)).rejects.toThrow(
    "injected activation failure",
  );
  await expect(readFile(join(target, "index.html"), "utf8")).resolves.toBe(
    compilationStub,
  );
});
