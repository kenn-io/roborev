// @vitest-environment node

import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, expect, test } from "vitest";

import { generateAPI } from "./generate-api";

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
  const webOutput = join(directory, "api", "generated");
  const packageOutput = join(directory, "package", "generated");
  await mkdir(join(directory, "api"), { recursive: true });
  await writeFile(
    join(directory, "api", "generated-fetch.ts"),
    [
      "export async function roborevFetch<T>(",
      "  _url: string,",
      "  _options: RequestInit,",
      "): Promise<T> {",
      '  throw new Error("not used");',
      "}",
      "",
    ].join("\n"),
  );
  await writeFile(
    spec,
    [
      "openapi: 3.1.0",
      "info:",
      "  title: fixture",
      "  version: 1.0.0",
      "paths:",
      "  /ping:",
      "    get:",
      "      operationId: ping",
      "      tags: [daemon]",
      "      responses:",
      '        "200":',
      "          description: OK",
      "          content:",
      "            application/json:",
      "              schema:",
      '                $ref: "#/components/schemas/Ping"',
      "components:",
      "  schemas:",
      "    Ping:",
      "      type: object",
      "      properties:",
      "        ok:",
      "          type: boolean",
      "      required: [ok]",
      "",
    ].join("\n"),
  );

  const options = { spec, webOutput, packageOutput, check: false };
  await generateAPI(options);
  const output = join(webOutput, "models", "ping.ts");
  const expected = await readFile(output, "utf8");
  await writeFile(output, "stale\n");

  await expect(generateAPI({ ...options, check: true })).rejects.toThrow(
    "generated browser API client is stale",
  );
  await expect(readFile(output, "utf8")).resolves.toBe("stale\n");

  await writeFile(output, expected);
  await expect(
    generateAPI({ ...options, check: true }),
  ).resolves.toBeUndefined();
});
