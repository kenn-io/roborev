// @vitest-environment node

import { mkdtemp, mkdir, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, expect, test } from "vitest";

import { validateAssetGraph } from "./validate-assets";

const temporaryDirectories: string[] = [];

afterEach(async () => {
  await Promise.all(
    temporaryDirectories
      .splice(0)
      .map((directory) => rm(directory, { recursive: true, force: true })),
  );
});

async function validDistribution(): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), "roborev-assets-"));
  temporaryDirectories.push(root);
  await mkdir(join(root, ".vite"), { recursive: true });
  await mkdir(join(root, "assets"), { recursive: true });
  await writeFile(
    join(root, "index.html"),
    '<meta name="roborev-web-distribution" content="production">',
  );
  await writeFile(
    join(root, ".vite/manifest.json"),
    JSON.stringify({
      "index.html": {
        file: "assets/index-a1b2c3.js",
        css: ["assets/index-a1b2c3.css"],
      },
    }),
  );
  await writeFile(join(root, "assets/index-a1b2c3.js"), "export {};\n");
  await writeFile(join(root, "assets/index-a1b2c3.css"), "body{}\n");
  return root;
}

test("accepts a complete Vite asset graph", async () => {
  await expect(
    validateAssetGraph(await validDistribution()),
  ).resolves.toBeUndefined();
});

test.each([
  [
    "missing production marker",
    async (root: string) => writeFile(join(root, "index.html"), "stub"),
  ],
  [
    "missing manifest",
    async (root: string) => rm(join(root, ".vite/manifest.json")),
  ],
  [
    "empty manifest",
    async (root: string) => writeFile(join(root, ".vite/manifest.json"), "{}"),
  ],
  [
    "missing referenced file",
    async (root: string) => rm(join(root, "assets/index-a1b2c3.js")),
  ],
] as const)("rejects %s", async (_name, mutate) => {
  const root = await validDistribution();
  await mutate(root);
  await expect(validateAssetGraph(root)).rejects.toThrow();
});

test("rejects symlinked assets", async () => {
  const root = await validDistribution();
  await rm(join(root, "assets/index-a1b2c3.js"));
  await symlink(join(root, "index.html"), join(root, "assets/index-a1b2c3.js"));
  await expect(validateAssetGraph(root)).rejects.toThrow("symlink");
});

test("rejects unsafe unreferenced distribution files", async () => {
  const root = await validDistribution();
  await writeFile(join(root, "token.json"), '{"value":"not-a-real-token"}');
  await expect(validateAssetGraph(root)).rejects.toThrow("secret-like");
});

test.each([
  "../outside.js",
  "assets/.hidden.js",
  "assets/token.js",
  "assets/key.pem",
])("rejects unsafe manifest path %s", async (assetPath) => {
  const root = await validDistribution();
  await writeFile(
    join(root, ".vite/manifest.json"),
    JSON.stringify({ "index.html": { file: assetPath } }),
  );
  await expect(validateAssetGraph(root)).rejects.toThrow();
});
