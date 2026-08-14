import { execFileSync } from "node:child_process";
import {
  mkdirSync,
  mkdtempSync,
  readdirSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";

import { afterEach, expect, test } from "vitest";

const temporaryDirectories: string[] = [];

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
}, 120_000);

test("the package ships only its supported source contract", () => {
  const destination = mkdtempSync(
    join(import.meta.dirname, ".roborev-ui-pack-"),
  );
  temporaryDirectories.push(destination);
  execFileSync("bun", ["pm", "pack", "--destination", destination], {
    cwd: import.meta.dirname,
    stdio: "pipe",
  });
  const archives = readdirSync(destination).filter((name) =>
    name.endsWith(".tgz"),
  );
  expect(archives).toHaveLength(1);

  const entries = execFileSync(
    "tar",
    ["-tf", join(destination, archives[0]!)],
    {
      encoding: "utf8",
    },
  )
    .trim()
    .split("\n")
    .filter(Boolean)
    .sort();

  expect(entries).toEqual([
    "package/README.md",
    "package/package.json",
    "package/src/components/PanelAttribution.svelte",
    "package/src/components/ResponseList.svelte",
    "package/src/components/ReviewContent.svelte",
    "package/src/components/ReviewMetadata.svelte",
    "package/src/components/ReviewProjectionView.svelte",
    "package/src/components/StatusBadge.svelte",
    "package/src/components/VerdictBadge.svelte",
    "package/src/generated.ts",
    "package/src/index.ts",
    "package/src/markdown/render.ts",
    "package/src/types.ts",
  ]);

  const consumer = join(destination, "consumer");
  mkdirSync(join(consumer, "src"), { recursive: true });
  const packageScope = join(consumer, "node_modules", "@kenn-io");
  const unpacked = join(destination, "unpacked");
  mkdirSync(packageScope, { recursive: true });
  mkdirSync(unpacked);
  execFileSync(
    "tar",
    ["-xf", join(destination, archives[0]!), "-C", unpacked],
    { stdio: "pipe" },
  );
  renameSync(join(unpacked, "package"), join(packageScope, "roborev-ui"));
  writeFileSync(
    join(consumer, "package.json"),
    JSON.stringify({
      private: true,
      type: "module",
    }),
  );
  writeFileSync(
    join(consumer, "index.html"),
    '<div id="app"></div><script type="module" src="/src/main.ts"></script>',
  );
  writeFileSync(
    join(consumer, "src", "main.ts"),
    [
      'import { mount } from "svelte";',
      'import { ReviewProjectionView } from "@kenn-io/roborev-ui";',
      "mount(ReviewProjectionView, {",
      '  target: document.querySelector("#app")!,',
      "  props: {",
      "    projection: {",
      "      schema_version: 1,",
      '      job: { id: 1, project: "project-a", git_ref: "abc", agent: "test", status: "done", enqueued_at: "2026-01-01T00:00:00Z" },',
      "      panel_members: [],",
      "      responses: [],",
      "    },",
      "  },",
      "});",
    ].join("\n"),
  );
  writeFileSync(
    join(consumer, "vite.config.ts"),
    'import { svelte } from "@sveltejs/vite-plugin-svelte";\nexport default { plugins: [svelte()] };\n',
  );
  execFileSync(
    join(import.meta.dirname, "node_modules", ".bin", "vite"),
    ["build"],
    {
      cwd: consumer,
      stdio: "pipe",
    },
  );
}, 120_000);
