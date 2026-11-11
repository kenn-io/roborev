<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { getReviewStores } from "../../stores/context";
  const stores = getReviewStores();
  const prompt = $derived(stores.roborevReview?.getPrompt());
  const loadFailure = $derived(stores.roborevReview?.getError());

  function retryReview(): void {
    const jobId = stores.roborevReview?.getSelectedJobId();
    if (jobId !== undefined) stores.roborevReview?.loadReview(jobId);
  }
</script>

<div class="prompt-viewer">
  {#if prompt}
    <pre class="prompt-text">{prompt}</pre>
  {:else if stores.roborevReview?.isLoading()}
    <div class="prompt-empty">Loading prompt…</div>
  {:else if loadFailure}
    <div class="prompt-empty prompt-retry" role="alert">
      <p>{loadFailure}</p>
      <Button size="sm" onclick={retryReview} label="Retry" />
    </div>
  {:else}
    <div class="prompt-empty">No prompt available.</div>
  {/if}
</div>

<style>
  .prompt-viewer {
    height: 100%;
    overflow-y: auto;
    background: var(--bg-inset);
    padding: 12px 16px;
  }

  .prompt-text {
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    line-height: 1.5;
    color: var(--text-primary);
    white-space: pre-wrap;
    word-break: break-word;
    margin: 0;
  }

  .prompt-empty {
    padding: 24px;
    text-align: center;
    font-size: var(--font-size-md);
    color: var(--text-muted);
  }

  /* Matches the review tab's error row: message and Retry share one line. */
  .prompt-retry {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: center;
    gap: var(--space-2) var(--space-3);
  }

  .prompt-retry p {
    margin: 0;
  }
</style>
