<script lang="ts">
  import { onMount } from "svelte";
  import type { Component } from "svelte";
  import { Button } from "@kenn-io/kit-ui";
  import { getReviewStores } from "../../stores/context";

  type RichReviewContentComponent = Component<{
    output: string;
    loading?: boolean;
    pending?: boolean;
  }>;

  let { visibleJobId = $bindable<number | undefined>() } = $props();

  const stores = getReviewStores();
  const output = $derived(stores.roborevReview?.getOutput() ?? "");
  const selectedJob = $derived(stores.roborevReview?.getSelectedJob());
  const loadFailure = $derived(stores.roborevReview?.getError());
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

  function reportRendererFailure(): void {
    stores.roborevReview?.reportRendererFailure();
  }
  function retryReview(): void {
    const jobId = stores.roborevReview?.getSelectedJobId();
    if (jobId !== undefined) stores.roborevReview?.loadReview(jobId);
  }
  function retryRenderer(): void {
    stores.roborevReview?.retryRenderer();
    void loadRichReviewContent();
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

  // Effects run after the rich component has rendered into the DOM.
  $effect(() => {
    visibleJobId =
      RichReviewContent &&
      output &&
      !stores.roborevReview?.isLoading() &&
      !loadFailure &&
      !failed &&
      !pending
        ? stores.roborevReview?.getReview()?.job_id
        : undefined;
  });

  $effect(() => {
    if (output && !RichReviewContent) void loadRichReviewContent();
  });
</script>

{#if stores.roborevReview?.isLoading()}
  <div class="review-state">Loading review…</div>
{:else if loadFailure}
  <div class="review-state review-retry" role="alert">
    <p>{loadFailure}</p>
    <Button size="sm" onclick={retryReview} label="Retry" />
  </div>
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
  <div
    class="review-state review-retry"
    role="alert"
    {@attach reportRendererFailure}
  >
    <p>Could not display the review.</p>
    <Button size="sm" onclick={retryRenderer} label="Retry" />
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

  /* The message and its Retry share one row so the error is no taller than
   * the empty state it replaces; otherwise the comment box below is pushed
   * under the dock footer at the default drawer height. */
  .review-retry {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: center;
    gap: var(--space-2) var(--space-3);
  }

  .review-retry p {
    margin: 0;
  }

  .review-failure p {
    margin: var(--space-2) 0 0;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
