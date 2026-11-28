<script lang="ts">
  import { overlayScrollbars } from "@kenn-io/kit-ui";
  import { getReviewStores } from "../../stores/context";
  const stores = getReviewStores();
  let frame = $state<HTMLDivElement>();
</script>

<div class="prompt-frame" bind:this={frame}>
  <div
    class="prompt-viewer"
    {@attach overlayScrollbars({ axis: "y", host: frame })}
  >
    {#if stores.roborevReview?.getPrompt()}
      <pre class="prompt-text">{stores.roborevReview.getPrompt()}</pre>
    {:else}
      <div class="prompt-empty">No prompt available.</div>
    {/if}
  </div>
</div>

<style>
  .prompt-frame {
    position: relative;
    flex: 1;
    min-height: 0;
    height: 100%;
    overflow: clip;
    background: var(--bg-inset);
  }

  .prompt-viewer {
    height: 100%;
    overflow-y: auto;
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
