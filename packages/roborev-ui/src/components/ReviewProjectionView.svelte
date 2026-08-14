<script lang="ts">
  import { supportsReviewProjection, type ReviewProjection } from "../types";
  import PanelAttribution from "./PanelAttribution.svelte";
  import ResponseList from "./ResponseList.svelte";
  import ReviewContent from "./ReviewContent.svelte";
  import ReviewMetadata from "./ReviewMetadata.svelte";

  interface Props {
    projection: ReviewProjection;
  }

  let { projection }: Props = $props();
</script>

{#if supportsReviewProjection(projection)}
  <article class="review-projection">
    <ReviewMetadata job={projection.job} />
    <PanelAttribution
      panelName={projection.job.panel_name}
      members={projection.panel_members ?? []}
    />
    <ReviewContent
      output={projection.review?.output ?? ""}
      pending={!projection.review &&
        !["done", "failed", "canceled", "skipped"].includes(
          projection.job.status,
        )}
    />
    <section class="responses-section" aria-label="Review comments">
      <ResponseList responses={projection.responses ?? []} />
    </section>
  </article>
{:else}
  <section
    class="unsupported-projection"
    role="alert"
    aria-label="Unsupported review data"
  >
    <strong>Unsupported review data</strong>
    <p>Update Roborev to display this review.</p>
  </section>
{/if}

<style>
  .review-projection {
    overflow: hidden;
    border: 1px solid var(--border-default, #30363d);
    border-radius: var(--radius-md, 6px);
    background: var(--bg-primary, #0d1117);
    color: var(--text-primary, #f0f6fc);
  }

  .responses-section {
    padding: 12px 16px 16px;
    border-top: 1px solid var(--border-default, #30363d);
  }

  .unsupported-projection {
    padding: 16px;
    border: 1px solid var(--border-default, #30363d);
    border-radius: var(--radius-md, 6px);
    background: var(--bg-primary, #0d1117);
    color: var(--text-primary, #f0f6fc);
  }

  .unsupported-projection p {
    margin: 6px 0 0;
    color: var(--text-secondary, #c9d1d9);
  }
</style>
