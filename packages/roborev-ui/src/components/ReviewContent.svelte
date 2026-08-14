<script lang="ts">
  import { initMarkdownMermaidRendering } from "@kenn-io/kit-ui/utils/markdown-mermaid";
  import { renderMarkdown, renderMarkdownSync } from "../markdown/render";

  interface Props {
    output: string;
    loading?: boolean;
    pending?: boolean;
    emptyMessage?: string;
  }

  let {
    output,
    loading = false,
    pending = false,
    emptyMessage = "No review output available.",
  }: Props = $props();

  const fallbackHTML = $derived(renderMarkdownSync(output));
  let highlighted = $state.raw<{ source: string; html: string } | null>(null);
  const renderedHTML = $derived(
    highlighted?.source === output ? highlighted.html : fallbackHTML,
  );
  let markdownContainer = $state<HTMLElement>();

  $effect(() => {
    const container = markdownContainer;
    const html = renderedHTML;
    if (!container || !html) return;
    const controller = initMarkdownMermaidRendering(container);
    controller.renderNow();
    return () => controller.disconnect();
  });

  $effect(() => {
    const source = output;
    if (!source) return;
    let current = true;
    void renderMarkdown(source).then((html) => {
      if (current) highlighted = { source, html };
    });
    return () => {
      current = false;
    };
  });
</script>

{#if loading}
  <div class="review-loading">Loading review...</div>
{:else if pending}
  <div class="review-pending">Review in progress...</div>
{:else if output}
  <div class="review-content markdown-body" bind:this={markdownContainer}>
    <!-- eslint-disable-next-line svelte/no-at-html-tags -- sanitized by the Markdown renderer -->
    {@html renderedHTML}
  </div>
{:else}
  <div class="review-empty">{emptyMessage}</div>
{/if}

<style>
  .review-loading,
  .review-pending,
  .review-empty {
    padding: 24px;
    text-align: center;
    font-size: var(--font-size-md);
    color: var(--text-muted);
  }

  .review-content {
    padding: 16px 20px;
  }

  /* Markdown prose styling */
  .markdown-body {
    font-size: var(--font-size-md);
    line-height: 1.6;
    color: var(--text-primary);
    word-wrap: break-word;
    overflow-wrap: break-word;
  }

  .markdown-body :global(h1) {
    font-size: var(--font-size-xl);
    font-weight: 600;
    margin: 20px 0 10px;
    padding-bottom: 6px;
    border-bottom: 1px solid var(--border-muted);
  }

  .markdown-body :global(h2) {
    font-size: var(--font-size-lg);
    font-weight: 600;
    margin: 18px 0 8px;
    padding-bottom: 4px;
    border-bottom: 1px solid var(--border-muted);
  }

  .markdown-body :global(h3) {
    font-size: var(--font-size-lg);
    font-weight: 600;
    margin: 16px 0 6px;
  }

  .markdown-body :global(h4),
  .markdown-body :global(h5),
  .markdown-body :global(h6) {
    font-size: var(--font-size-md);
    font-weight: 600;
    margin: 14px 0 4px;
  }

  .markdown-body :global(p) {
    margin: 0 0 10px;
  }

  .markdown-body :global(ul),
  .markdown-body :global(ol) {
    margin: 0 0 10px;
    padding-left: 24px;
  }

  .markdown-body :global(li) {
    margin-bottom: 4px;
  }

  .markdown-body :global(li > ul),
  .markdown-body :global(li > ol) {
    margin-bottom: 0;
  }

  .markdown-body :global(blockquote) {
    margin: 0 0 10px;
    padding: 4px 12px;
    border-left: 3px solid var(--border-default);
    color: var(--text-secondary);
  }

  .markdown-body :global(code) {
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    padding: 2px 5px;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }

  .markdown-body :global(pre) {
    margin: 0 0 10px;
    padding: 12px;
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    overflow-x: auto;
  }

  .markdown-body :global(pre code) {
    padding: 0;
    background: none;
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }

  .markdown-body :global(table) {
    width: 100%;
    border-collapse: collapse;
    margin: 0 0 10px;
    font-size: var(--font-size-sm);
  }

  .markdown-body :global(th),
  .markdown-body :global(td) {
    padding: 6px 10px;
    border: 1px solid var(--border-muted);
    text-align: left;
  }

  .markdown-body :global(th) {
    font-weight: 600;
    background: var(--bg-inset);
  }

  .markdown-body :global(hr) {
    margin: 16px 0;
    border: none;
    border-top: 1px solid var(--border-muted);
  }

  .markdown-body :global(a) {
    color: var(--accent-blue);
    text-decoration: none;
  }

  .markdown-body :global(a:hover) {
    text-decoration: underline;
  }

  .markdown-body :global(img) {
    max-width: 100%;
  }

  .markdown-body :global(strong) {
    font-weight: 600;
  }
</style>
