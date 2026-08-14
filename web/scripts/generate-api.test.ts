// @vitest-environment node

import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, expect, test } from "vitest";

import { generateAPI, type GeneratorRunner } from "./generate-api";

const temporaryDirectories: string[] = [];

afterEach(async () => {
  await Promise.all(
    temporaryDirectories
      .splice(0)
      .map((directory) => rm(directory, { recursive: true, force: true })),
  );
});

test("check mode detects drift without changing the output", async () => {
  const directory = await mkdtemp(join(tmpdir(), "roborev-api-generate-"));
  temporaryDirectories.push(directory);
  const spec = join(directory, "openapi.yaml");
  const output = join(directory, "generated.ts");
  await writeFile(
    spec,
    [
      "openapi: 3.0.3",
      "info:",
      "  title: fixture",
      "  version: 1.0.0",
      "paths: {}",
      "",
    ].join("\n"),
  );

  const runner: GeneratorRunner = async (_spec, destination) => {
    await writeFile(destination, "generated\n");
  };

  await generateAPI({ spec, output, check: false }, runner);
  const expected = await readFile(output, "utf8");
  await writeFile(output, "stale\n");

  await expect(
    generateAPI({ spec, output, check: true }, runner),
  ).rejects.toThrow("generated browser API types are stale");
  await expect(readFile(output, "utf8")).resolves.toBe("stale\n");

  await writeFile(output, expected);
  await expect(
    generateAPI({ spec, output, check: true }, runner),
  ).resolves.toBeUndefined();
});
