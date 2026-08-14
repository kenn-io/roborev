<script lang="ts">
  import { getReviewStores } from "../../stores/context";
  const stores = getReviewStores();
</script>

<div class="prompt-viewer">
  {#if stores.roborevReview?.getPrompt()}
    <pre class="prompt-text">{stores.roborevReview.getPrompt()}</pre>
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
</style>
