<script lang="ts">
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import type { ReviewJob } from "../../api/generated/models";
  import {
    panelCostUsd,
    panelElapsedStart,
    panelStatusLabel,
  } from "../../utils/roborev-panel";
  import { reviewTypeLabel } from "../../utils/roborev-review-type";
  import { formatRelativeTime } from "@kenn-io/kit-ui";
  import StatusBadge from "./StatusBadge.svelte";
  import VerdictBadge from "./VerdictBadge.svelte";

  interface Props {
    job: ReviewJob;
    selected: boolean;
    highlighted: boolean;
    onclick: () => void;
    members?: ReviewJob[] | undefined;
    member?: boolean;
    expandable?: boolean;
    expanded?: boolean;
    ontoggle?: (() => void) | undefined;
  }
  let {
    job,
    selected,
    highlighted,
    onclick,
    members,
    member = false,
    expandable = false,
    expanded = false,
    ontoggle,
  }: Props = $props();

  const panelStatus = $derived(panelStatusLabel(job));
  const reviewType = $derived(reviewTypeLabel(job.review_type, job.panel_role));

  function formatElapsed(j: ReviewJob): string {
    const startedAt = panelElapsedStart(j, members);
    if (!startedAt) return "--";
    const start = new Date(startedAt).getTime();
    const end = j.finished_at ? new Date(j.finished_at).getTime() : Date.now();
    const secs = Math.floor((end - start) / 1000);
    if (secs < 60) return `${secs}s`;
    const mins = Math.floor(secs / 60);
    const remSecs = secs % 60;
    if (mins < 60) return `${mins}m ${remSecs}s`;
    const hrs = Math.floor(mins / 60);
    const remMins = mins % 60;
    return `${hrs}h ${remMins}m`;
  }

  function formatCost(j: ReviewJob): string {
    const cost = panelCostUsd(j, members);
    if (cost === null) return "--";
    return `~$${cost.toFixed(2)}`;
  }

  function shortRef(ref: string): string {
    if (ref.length > 10) return ref.slice(0, 8);
    return ref;
  }

  function closedLabel(j: ReviewJob): string {
    if (member || j.panel_role === "member" || j.closed === undefined) {
      return "--";
    }
    return j.closed ? "yes" : "no";
  }
</script>

<tr
  class="job-row"
  class:selected
  class:highlighted
  class:member
  aria-expanded={expandable ? expanded : undefined}
  role="button"
  tabindex="0"
  {onclick}
  onkeydown={(e) => {
    if (e.key === "Enter" || e.key === " ") onclick();
  }}
>
  <td class="col-id">
    <span class="job-id">{job.id}</span>
  </td>
  <td class="col-ref" class:tree-cell={expandable || member}>
    <span class="ref-line" class:ref-line--member={member}>
      {#if expandable}
        <button
          class="chevron"
          class:open={expanded}
          type="button"
          tabindex="-1"
          aria-label={expanded ? "Collapse panel" : "Expand panel"}
          onclick={(e) => {
            e.stopPropagation();
            ontoggle?.();
          }}
        >
          <ChevronRightIcon size={12} strokeWidth={2} aria-hidden="true" />
        </button>
      {:else if member}
        <span class="tree-spacer" aria-hidden="true"></span>
      {/if}
      <span class="ref-stack">
        <span class="ref-group">
          {#if job.repo_name}
            <span class="repo-name">{job.repo_name}</span>
          {/if}
          {#if job.branch}
            <span class="branch-name" title={job.branch}>{job.branch}</span>
          {/if}
          <span class="git-ref" title={job.git_ref}>
            {shortRef(job.git_ref)}
          </span>
        </span>
        {#if job.commit_subject}
          <span class="commit-subject" title={job.commit_subject}>
            {job.commit_subject}
          </span>
        {/if}
        {#if member && job.panel_member_name}
          <span class="member-name"
            >{job.panel_member_name}{job.non_voting
              ? " (non-voting)"
              : ""}</span
          >
        {/if}
        {#if panelStatus}
          <span class="panel-status">{panelStatus}</span>
        {/if}
      </span>
    </span>
  </td>
  <td class="col-agent">{job.agent}</td>
  <td class="col-review-type" title={reviewType}>{reviewType}</td>
  <td class="col-status">
    <StatusBadge status={job.status} />
  </td>
  <td class="col-verdict">
    <VerdictBadge verdict={job.verdict} />
  </td>
  <td class="col-closed">{closedLabel(job)}</td>
  <td class="col-elapsed">
    {formatElapsed(job)}
  </td>
  <td class="col-cost">
    {formatCost(job)}
  </td>
  <td class="col-type">{job.job_type}</td>
  <td class="col-queued" title={job.enqueued_at}>
    {formatRelativeTime(job.enqueued_at)}
  </td>
</tr>

<style>
  .job-row {
    cursor: pointer;
    transition: background var(--transition-fast);
  }

  .job-row:hover {
    background: var(--bg-surface-hover);
  }

  .job-row:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .job-row.highlighted {
    background: color-mix(in srgb, var(--accent-blue) 5%, var(--bg-surface));
    box-shadow: inset 2px 0 0
      color-mix(in srgb, var(--accent-blue) 55%, transparent);
  }

  .job-row.selected {
    background: color-mix(in srgb, var(--accent-blue) 10%, var(--bg-surface));
    box-shadow: inset 2px 0 0 var(--accent-blue);
  }

  .job-row.member:not(.selected, .highlighted, :hover) {
    background: var(--bg-inset);
  }

  .job-row td {
    padding: 8px 12px;
    border-bottom: 1px solid var(--border-muted);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-variant-numeric: tabular-nums;
    vertical-align: middle;
    white-space: nowrap;
  }

  .job-row td.col-id {
    width: 60px;
    padding-left: 16px;
    color: var(--text-muted);
    text-align: right;
  }

  .chevron {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    width: 16px;
    height: 16px;
    margin-top: 1px;
    padding: 0;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    transition:
      transform var(--transition-fast),
      background var(--transition-fast),
      color var(--transition-fast);
  }

  .chevron:hover {
    background: var(--bg-inset);
    color: var(--text-primary);
  }

  .chevron.open {
    transform: rotate(90deg);
    color: var(--text-primary);
  }

  .col-ref {
    min-width: 200px;
    max-width: 380px;
    white-space: normal;
  }

  .ref-line {
    display: flex;
    align-items: flex-start;
    gap: var(--space-3);
    min-width: 0;
  }

  .tree-cell .ref-line--member {
    padding-left: 20px;
  }

  .tree-spacer {
    flex: 0 0 16px;
    width: 16px;
    height: 16px;
  }

  .ref-stack {
    display: grid;
    gap: 2px;
    min-width: 0;
  }

  .ref-group {
    display: flex;
    align-items: baseline;
    gap: var(--space-3);
    min-width: 0;
    white-space: nowrap;
  }

  .repo-name {
    font-weight: 500;
  }

  .branch-name {
    overflow: hidden;
    min-width: 0;
    color: var(--text-secondary);
    text-overflow: ellipsis;
  }

  .repo-name,
  .git-ref {
    flex-shrink: 0;
  }

  .branch-name::before {
    margin-right: var(--space-3);
    color: var(--text-muted);
    content: "/";
  }

  .git-ref {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }

  .commit-subject {
    overflow: hidden;
    max-width: 360px;
    color: var(--text-secondary);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .member-name {
    color: var(--text-primary);
    font-weight: 500;
  }

  .panel-status {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .col-agent {
    max-width: 120px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .col-review-type {
    max-width: 160px;
    overflow: hidden;
    color: var(--text-secondary);
    text-overflow: ellipsis;
  }

  .col-closed,
  .col-type {
    color: var(--text-secondary);
  }

  .col-elapsed,
  .col-cost {
    color: var(--text-secondary);
    text-align: right;
  }

  .col-queued {
    color: var(--text-muted);
    text-align: right;
  }
</style>
