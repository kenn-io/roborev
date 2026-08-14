<script lang="ts">
  import type { ReviewProjectionJob } from "../types";
  import StatusBadge from "./StatusBadge.svelte";
  import VerdictBadge from "./VerdictBadge.svelte";

  interface Props {
    job: ReviewProjectionJob;
  }

  let { job }: Props = $props();
  const shortRef = $derived(
    job.git_ref.length > 10 ? job.git_ref.slice(0, 8) : job.git_ref,
  );
</script>

<header class="review-metadata">
  <div class="identity">
    <span class="job-id">#{job.id}</span>
    <VerdictBadge verdict={job.verdict} />
    <strong>{job.project}</strong>
    {#if job.branch}<span>{job.branch}</span>{/if}
    <span title={job.git_ref}>{shortRef}</span>
    <StatusBadge status={job.status} />
  </div>
  <div class="details">
    <span>{job.agent}{job.model ? ` / ${job.model}` : ""}</span>
    {#if job.commit_subject}<span>{job.commit_subject}</span>{/if}
    {#if job.review_type}<span>{job.review_type}</span>{/if}
  </div>
</header>

<style>
  .review-metadata {
    display: grid;
    gap: 6px;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border-default, #30363d);
    color: var(--text-primary, #f0f6fc);
  }

  .identity,
  .details {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 10px;
  }

  .details,
  .job-id {
    color: var(--text-secondary, #c9d1d9);
    font-size: var(--font-size-sm, 0.8125rem);
  }
</style>
