<script lang="ts">
  import { onMount } from "svelte";
  import type { Component } from "svelte";
  import { getReviewStores } from "../../stores/context";

  type RichReviewContentComponent = Component<{
    output: string;
    loading?: boolean;
    pending?: boolean;
  }>;

  const stores = getReviewStores();
  const output = $derived(stores.roborevReview?.getOutput() ?? "");
  const selectedJob = $derived(stores.roborevReview?.getSelectedJob());
  const failed = $derived(selectedJob?.status === "failed");
  const failureReason = $derived(
    selectedJob?.error?.trim() ||
      "The review agent failed before producing output.",
  );
  const pending = $derived.by(() => {
    if (!stores.roborevReview?.isReviewNotFound()) return false;
    const status = selectedJob?.status;
    return status === "queued" || status === "running";
  });
  let RichReviewContent = $state<RichReviewContentComponent>();
  let loadError = $state(false);
  let loadPromise: Promise<void> | undefined;

  async function loadRichReviewContent(): Promise<void> {
    if (RichReviewContent) return;
    if (loadPromise) return loadPromise;
    loadError = false;
    loadPromise = import("@kenn-io/roborev-ui/review-content")
      .then((module) => {
        RichReviewContent = module.default;
      })
      .catch(() => {
        loadError = true;
      })
      .finally(() => {
        loadPromise = undefined;
      });
    return loadPromise;
  }

  onMount(() => {
    if ("requestIdleCallback" in globalThis) {
      const request = globalThis.requestIdleCallback(() => {
        void loadRichReviewContent();
      });
      return () => globalThis.cancelIdleCallback(request);
    }
    const timeout = globalThis.setTimeout(() => {
      void loadRichReviewContent();
    }, 0);
    return () => globalThis.clearTimeout(timeout);
  });

  $effect(() => {
    if (output && !RichReviewContent) void loadRichReviewContent();
  });
</script>

{#if stores.roborevReview?.isLoading()}
  <div class="review-state">Loading review…</div>
{:else if failed}
  <div class="review-state review-failure" role="alert">
    <strong>Review failed</strong>
    <p>{failureReason}</p>
  </div>
{:else if RichReviewContent}
  <RichReviewContent
    {output}
    loading={stores.roborevReview?.isLoading() ?? false}
    {pending}
  />
{:else if loadError && output}
  <div class="review-state" role="alert">
    <p>Could not load the review renderer.</p>
    <button type="button" onclick={() => void loadRichReviewContent()}>
      Retry
    </button>
  </div>
{:else if pending}
  <div class="review-state">Review in progress…</div>
{:else if output}
  <div class="review-state">Rendering review…</div>
{:else}
  <div class="review-state">No review output available.</div>
{/if}

<style>
  .review-state {
    padding: 24px;
    text-align: center;
    font-size: var(--font-size-md);
    color: var(--text-muted);
  }

  .review-state p {
    margin: 0 0 var(--space-3);
  }

  .review-failure p {
    margin: var(--space-2) 0 0;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }

  .review-state button {
    padding: 5px 10px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    cursor: pointer;
  }

  .review-state button:hover {
    background: var(--bg-surface-hover);
  }
</style>
