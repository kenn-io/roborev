import { describe, expect, it } from "vitest";
import {
  renderMarkdown,
  renderMarkdownBlocks,
  renderMarkdownSync,
} from "./render";

describe("Roborev markdown rendering", () => {
  it("renders disabled checkboxes by default", async () => {
    const html = await renderMarkdown("- [ ] one\n- [x] two");
    expect(html).toContain('disabled=""');
    expect(html).not.toContain("data-task-index");
  });

  it("renders enabled checkboxes with sequential indices when interactiveTasks is set", async () => {
    const html = await renderMarkdown(
      "- [ ] alpha\n- [x] beta\n- [ ] gamma",
      undefined,
      {
        interactiveTasks: true,
      },
    );
    expect(html).not.toContain('disabled=""');
    expect(html).toContain('data-task-index="0"');
    expect(html).toContain('data-task-index="1"');
    expect(html).toContain('data-task-index="2"');
  });

  it("starts the task index at zero for every render", async () => {
    const opts = { interactiveTasks: true } as const;
    const first = await renderMarkdown("- [ ] a", undefined, opts);
    const second = await renderMarkdown("- [ ] b", undefined, opts);
    expect(first).toContain('data-task-index="0"');
    expect(second).toContain('data-task-index="0"');
  });

  it("preserves checked state when interactiveTasks is set", async () => {
    const html = await renderMarkdown("- [x] done", undefined, {
      interactiveTasks: true,
    });
    expect(html).toContain('checked=""');
  });

  it("caches interactive and non-interactive renders separately", async () => {
    const src = "- [ ] task";
    const plain = await renderMarkdown(src);
    const interactive = await renderMarkdown(src, undefined, {
      interactiveTasks: true,
    });
    expect(plain).toContain('disabled=""');
    expect(interactive).toContain('data-task-index="0"');
  });

  it("emits a drag handle and item-level data-task-index for interactive tasks", async () => {
    const html = await renderMarkdown("- [ ] a\n- [ ] b", undefined, {
      interactiveTasks: true,
    });
    expect(html).toContain(
      '<li class="task-list-item task-list-item--interactive" data-task-index="0">',
    );
    expect(html).toContain(
      '<span class="task-drag-handle" data-task-index="0"',
    );
    expect(html).toContain(
      '<span class="task-drag-handle" data-task-index="1"',
    );
    expect(html).toContain('draggable="true"');
  });

  it("does not emit drag handles in non-interactive mode", async () => {
    const html = await renderMarkdown("- [ ] a");
    expect(html).not.toContain("task-drag-handle");
    expect(html).not.toContain("draggable");
  });

  it("emits only one input per task item in interactive mode", async () => {
    const html = await renderMarkdown("- [ ] a", undefined, {
      interactiveTasks: true,
    });
    const matches = html.match(/<input/g) ?? [];
    expect(matches.length).toBe(1);
  });

  it("renders blockquoted task items as non-interactive even when interactiveTasks is set", async () => {
    // Source-side TASK_LINE doesn't match `> - [ ]` so the renderer
    // must NOT emit interactive checkboxes for them — otherwise
    // data-task-index would drift from the source helpers and
    // clicking would mutate the wrong line.
    const html = await renderMarkdown(
      "> - [ ] inside blockquote\n\n- [ ] outside",
      undefined,
      {
        interactiveTasks: true,
      },
    );
    // The blockquoted checkbox stays disabled with no data-task-index.
    expect(html).toMatch(
      /<blockquote>[\s\S]*<input disabled="" type="checkbox">[\s\S]*<\/blockquote>/,
    );
    // The plain task outside the blockquote keeps interactivity at
    // index 0 (the blockquoted one didn't consume an index).
    expect(html).toContain('data-task-index="0"');
    expect(html).not.toContain('data-task-index="1"');
  });

  it("preserves per-listitem indices when task items are nested", async () => {
    // Each <li> and its drag handle MUST carry the same index as the
    // checkbox that lives directly inside that <li>. A nested child
    // must not leak its index back up to its parent's wrapper.
    const html = await renderMarkdown(
      "- [ ] outer\n  - [ ] inner\n- [x] sibling",
      undefined,
      {
        interactiveTasks: true,
      },
    );
    // The outer <li> wraps both the outer checkbox AND the nested
    // list — its data-task-index must match its OWN checkbox (0),
    // not the nested child's (1).
    expect(html).toContain(
      '<li class="task-list-item task-list-item--interactive" data-task-index="0">',
    );
    expect(html).toContain(
      '<li class="task-list-item task-list-item--interactive" data-task-index="1">',
    );
    expect(html).toContain(
      '<li class="task-list-item task-list-item--interactive" data-task-index="2">',
    );
    expect(html).toContain(
      '<span class="task-drag-handle" data-task-index="0"',
    );
    expect(html).toContain(
      '<span class="task-drag-handle" data-task-index="1"',
    );
    expect(html).toContain(
      '<span class="task-drag-handle" data-task-index="2"',
    );
    // Sanity-check pairing: the outer <li> contains the nested <li>
    // in its inner content, and the outer's drag handle precedes
    // the outer's checkbox.
    const outerOpen = html.indexOf(
      'data-task-index="0"><span class="task-drag-handle" data-task-index="0"',
    );
    expect(outerOpen).toBeGreaterThanOrEqual(0);
  });
});

describe("renderMarkdown mermaid diagrams", () => {
  it("renders mermaid fences as mermaid diagram targets", async () => {
    const html = await renderMarkdown("```mermaid\ngraph TD\n  A --> B\n```");

    expect(html).toContain('<pre class="mermaid">graph TD\n  A --&gt; B</pre>');
    expect(html).not.toContain("language-mermaid");
  });
});

describe("renderMarkdown code highlighting", () => {
  it("highlights fenced code blocks with Shiki", async () => {
    const html = await renderMarkdown(
      '```ts\nconst value: string = "ok";\n```',
    );

    expect(html).toContain(
      '<pre class="shiki shiki-themes github-light-default github-dark-default"',
    );
    expect(html).toContain("--shiki-light:");
    expect(html).toContain("--shiki-dark:");
    expect(html).toContain("const");
    expect(html).not.toContain("language-ts");
  });

  it("uses Shiki bundled languages beyond the app's common-language fixtures", async () => {
    const html = await renderMarkdown("```zig\nconst value: u8 = 1;\n```");

    expect(html).toContain(
      '<pre class="shiki shiki-themes github-light-default github-dark-default"',
    );
    expect(html).toContain("--shiki-light:");
    expect(html).toContain("--shiki-dark:");
    expect(html).toContain("value");
  });

  it("passes the fenced TOML language through to Shiki", async () => {
    const html = await renderMarkdown(
      '```toml\nmodel_provider = "my-custom"\n```',
    );

    expect(html).toContain(
      '<pre class="shiki shiki-themes github-light-default github-dark-default"',
    );
    expect(html).toContain("--shiki-light:");
    expect(html).toContain("--shiki-dark:");
    expect(html).toContain("model_provider");
    expect(html).not.toContain("language-toml");
  });

  it("strips user-authored inline styles while preserving Shiki theme variables", async () => {
    const html = await renderMarkdown(
      '<span style="position:fixed;color:red">raw</span>\n\n```ts\nconst value = 1;\n```',
    );

    expect(html).toContain("<span>raw</span>");
    expect(html).not.toContain("position:fixed");
    expect(html).not.toContain("color:red");
    expect(html).toContain("--shiki-light:");
    expect(html).toContain("--shiki-dark:");
  });

  it("removes raw style elements", async () => {
    const html = await renderMarkdown(
      "<svg><style>body { display: none }</style><text>Review output</text></svg>",
    );

    expect(html).not.toContain("<style");
    expect(html).not.toContain("display: none");
    expect(html).toContain("Review output");
  });

  it("isolates links that open a new browsing context", async () => {
    const html = await renderMarkdown(
      '<a href="https://example.com/review" target="_blank">details</a>',
    );

    expect(html).toContain('target="_blank"');
    expect(html).toContain('rel="noopener noreferrer"');
  });

  it("strips styles from raw HTML that forges Shiki class names", async () => {
    const html = await renderMarkdown(
      [
        '<pre class="shiki" style="--shiki-light:#000000;--shiki-dark:#000000">',
        '<span style="--shiki-light:#000000;--shiki-dark:#000000">forged</span>',
        "</pre>",
        "",
        "```ts",
        "const value = 1;",
        "```",
      ].join("\n"),
    );

    expect(html).toContain('<pre class="shiki">');
    expect(html).toContain("<span>forged</span>");
    expect(html).not.toContain("--shiki-light:#000000");
    expect(html).not.toContain("--shiki-dark:#000000");
    expect(html).not.toContain("data-kenn-forge-shiki");
    expect(html).toContain("--shiki-light:");
    expect(html).toContain("--shiki-dark:");
  });

  it("keeps synchronous block rendering explicitly unhighlighted after Shiki has loaded", async () => {
    await renderMarkdown("```ts\nconst value = 1;\n```");

    const blocks = renderMarkdownBlocks("```ts\nconst value = 1;\n```");

    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.html).toContain("<pre><code>");
    expect(blocks[0]?.html).not.toContain("shiki");
    expect(blocks[0]?.html).toContain("const value = 1;");
  });

  it("falls back to escaped plain text for unknown fence languages", async () => {
    const html = await renderMarkdown(
      "```not-a-real-language\n<script>alert(1)</script>\n```",
    );

    expect(html).toContain(
      '<pre class="shiki shiki-themes github-light-default github-dark-default"',
    );
    expect(html).toContain("&lt;script&gt;alert(1)&lt;/script&gt;");
  });

  it("falls back to plain code blocks after the per-render highlighted fence budget", async () => {
    const source = Array.from(
      { length: 21 },
      (_, index) => `\`\`\`ts\nconst value${index} = ${index};\n\`\`\``,
    ).join("\n\n");

    const html = await renderMarkdown(source);

    expect(html.match(/<pre class="shiki/g)).toHaveLength(20);
    expect(html.match(/<pre><code>/g)).toHaveLength(1);
    expect(html).toContain("const value20 = 20;");
  });

  it("falls back to plain code blocks after the per-render language budget", async () => {
    const languages = [
      "bash",
      "css",
      "diff",
      "go",
      "html",
      "json",
      "python",
      "rust",
      "ruby",
    ];
    const source = languages
      .map((lang, index) => `\`\`\`${lang}\nvalue_${index}\n\`\`\``)
      .join("\n\n");

    const html = await renderMarkdown(source);

    expect(html.match(/<pre class="shiki/g)).toHaveLength(8);
    expect(html.match(/<pre><code>/g)).toHaveLength(1);
    expect(html).toContain("value_8");
  });
});

describe("renderMarkdown details blocks", () => {
  const source = [
    "<details open>",
    "",
    "<summary>Tips for collapsed sections</summary>",
    "",
    "### You can add a header",
    "",
    "You can add text within a collapsed section.",
    "",
    "```ruby",
    'puts "Hello World"',
    "```",
    "",
    "</details>",
  ].join("\n");

  it("preserves GitHub-style details blocks with rendered markdown inside", async () => {
    const html = await renderMarkdown(source);

    expect(html).toContain("<details");
    expect(html).toContain('open=""');
    expect(html).toContain("<summary>Tips for collapsed sections</summary>");
    expect(html).toContain("<h3>You can add a header</h3>");
    expect(html).toContain(
      "<p>You can add text within a collapsed section.</p>",
    );
    expect(html).toContain(
      '<pre class="shiki shiki-themes github-light-default github-dark-default"',
    );
    expect(html).toContain("puts");
    expect(html).toContain('"Hello World"');
    expect(html).toContain("</details>");
  });

  it("keeps details blocks as one rendered block for rich markdown previews", async () => {
    const blocks = renderMarkdownBlocks(source);

    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.startLine).toBe(1);
    expect(blocks[0]?.endLine).toBe(13);
    expect(blocks[0]?.html).toContain("<details");
    expect(blocks[0]?.html).toContain(
      "<summary>Tips for collapsed sections</summary>",
    );
    expect(blocks[0]?.html).toContain("<h3>You can add a header</h3>");
    expect(blocks[0]?.html).toContain("</details>");
  });

  it("does not treat details tags inside fenced code as block boundaries", async () => {
    const blocks = renderMarkdownBlocks(
      [
        "<details>",
        "",
        "<summary>Markup sample</summary>",
        "",
        "```html",
        "</details>",
        "```",
        "",
        "Still inside the collapsed section.",
        "",
        "</details>",
        "",
        "Afterwards.",
      ].join("\n"),
    );

    expect(blocks).toHaveLength(2);
    expect(blocks[0]?.html).toContain("<details>");
    expect(blocks[0]?.html).toContain("&lt;/");
    expect(blocks[0]?.html).toContain("details");
    expect(blocks[0]?.html).toContain("&gt;");
    expect(blocks[0]?.html).toContain(
      "<p>Still inside the collapsed section.</p>",
    );
    expect(blocks[0]?.html).toContain("</details>");
    expect(blocks[0]?.html).not.toContain("Afterwards.");
    expect(blocks[1]?.html).toContain("<p>Afterwards.</p>");
  });

  it("keeps safe image sources and rejects script URLs", async () => {
    const safe = await renderMarkdown(
      "![diagram](https://example.com/diagram.png)",
    );
    const data = await renderMarkdown(
      "![pixel](data:image/png;base64,iVBORw0KGgo=)",
    );
    const unsafe = await renderMarkdown("![bad](javascript:alert(1))");
    expect(safe).toContain('src="https://example.com/diagram.png"');
    expect(data).toContain('src="data:image/png;base64,iVBORw0KGgo="');
    expect(unsafe).not.toContain("javascript:");
  });

  it("keeps synchronous fallback rendering available before highlighting", () => {
    expect(renderMarkdownSync("**review**")).toContain(
      "<strong>review</strong>",
    );
  });
});
