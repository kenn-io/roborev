<script lang="ts">
  import { StatusDot } from "@kenn-io/kit-ui";
  import { getReviewStores } from "../../stores/context";

  interface Props {
    jobId: number;
    jobStatus: string;
  }
  let { jobId, jobStatus }: Props = $props();

  const stores = getReviewStores();
  const logStore = stores.roborevLog;
  let container: HTMLElement | undefined;
  const lines = $derived(logStore?.getLines() ?? []);
  const lineCount = $derived(lines.length);
  const isStreaming = $derived(logStore?.isStreaming() ?? false);
  const followMode = $derived(logStore?.getFollowMode() ?? false);

  $effect(() => {
    if (!logStore) return;
    logStore.clear();
    const logOwner =
      jobStatus === "running" || jobStatus === "queued"
        ? logStore.startStreaming(jobId)
        : logStore.loadSnapshot(jobId);
    return () => logStore.stopStreaming(logOwner);
  });

  $effect(() => {
    if (!logStore || !container) return;
    void lineCount;
    if (followMode) {
      container.scrollTop = container.scrollHeight;
    }
  });

  function lineClass(lineType: string): string {
    if (lineType === "stderr") return "line-stderr";
    return "";
  }
</script>

<div class="log-viewer">
  <div class="log-toolbar">
    <span class="log-status">
      {#if isStreaming}
        <StatusDot status="working" label="Streaming review log" size={6} />
        <span aria-hidden="true">Streaming...</span>
      {:else}
        {lineCount} lines
      {/if}
    </span>
    <button
      class="follow-btn"
      class:active={followMode}
      onclick={() => logStore?.toggleFollow()}
      title="Auto-scroll to bottom"
    >
      Follow
    </button>
  </div>
  <div class="log-container" bind:this={container}>
    {#if logStore}
      {#each lines as line, index (`${line.ts}:${index}`)}
        <div class="log-line {lineClass(line.lineType)}">
          <span class="log-text">{line.text}</span>
        </div>
      {/each}
      {#if !isStreaming && lineCount === 0}
        <div class="log-empty">No log output available.</div>
      {/if}
    {:else}
      <div class="log-empty">Log store not available.</div>
    {/if}
  </div>
</div>

<style>
  .log-viewer {
    display: flex;
    flex-direction: column;
    height: 100%;
  }

  .log-toolbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 6px 12px;
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
    font-size: var(--font-size-sm);
  }

  .log-status {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--text-secondary);
  }

  .follow-btn {
    padding: 2px 10px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    cursor: pointer;
  }

  .follow-btn:hover {
    background: var(--bg-surface-hover);
  }

  .follow-btn.active {
    background: var(--accent-blue);
    color: var(--text-on-accent);
    border-color: var(--accent-blue);
  }

  .log-container {
    flex: 1;
    overflow-y: auto;
    background: var(--bg-inset);
    padding: 8px 12px;
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }

  .log-line {
    white-space: pre-wrap;
    word-break: break-all;
    color: var(--text-primary);
  }

  .line-stderr {
    color: var(--review-failed);
  }

  .log-text {
    user-select: text;
  }

  .log-empty {
    padding: 24px;
    text-align: center;
    color: var(--text-muted);
    font-family: var(--font-sans);
    font-size: var(--font-size-md);
  }
</style>
