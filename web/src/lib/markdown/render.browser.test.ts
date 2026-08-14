import { readFileSync } from "node:fs";
import { describe, expect, test } from "vitest";

import { renderMarkdown } from "./render";

describe("Markdown browser contract", () => {
  test("keeps long task labels in normal block flow", () => {
    const css = readFileSync("src/app.css", "utf8");
    const rule = css.match(/\.markdown-body \.task-list-item\s*\{([^}]*)\}/);

    expect(rule?.[1]).toContain("display: block");
    expect(rule?.[1]).not.toContain("display: flex");
  });

  test("removes the internal Shiki trust marker after sanitizing styles", async () => {
    const html = await renderMarkdown("```ts\nconst answer = 42;\n```");

    expect(html).toContain('class="shiki');
    expect(html).toContain("--shiki-light:");
    expect(html).not.toContain("data-roborev-shiki");
  });
});
