<script lang="ts">
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import type { components } from "../../api/generated";
  import {
    panelCostUsd,
    panelElapsedStart,
    panelStatusLabel,
  } from "../../utils/roborev-panel";
  import { formatRelativeTime } from "@kenn-io/kit-ui";
  import StatusBadge from "./StatusBadge.svelte";
  import VerdictBadge from "./VerdictBadge.svelte";

  type ReviewJob = components["schemas"]["ReviewJob"];

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
    <span class="mono">{job.id}</span>
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
            <span class="branch-name">{job.branch}</span>
          {/if}
          <span class="git-ref mono" title={job.git_ref}>
            {shortRef(job.git_ref)}
          </span>
        </span>
        {#if job.commit_subject}
          <span class="commit-subject" title={job.commit_subject}>
            {job.commit_subject}
          </span>
        {/if}
        {#if member && job.panel_member_name}
          <span class="member-name">{job.panel_member_name}</span>
        {/if}
        {#if panelStatus}
          <span class="panel-status">{panelStatus}</span>
        {/if}
      </span>
    </span>
  </td>
  <td class="col-agent">{job.agent}</td>
  <td class="col-status">
    <StatusBadge status={job.status} />
  </td>
  <td class="col-verdict">
    <VerdictBadge verdict={job.verdict} />
  </td>
  <td class="col-elapsed mono">
    {formatElapsed(job)}
  </td>
  <td class="col-cost mono">
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
    border-bottom: 1px solid var(--border-muted);
    transition: background 0.1s;
  }

  .job-row:hover {
    background: var(--bg-surface-hover);
  }

  .job-row.highlighted {
    background: color-mix(in srgb, var(--accent-blue) 4%, var(--bg-surface));
    outline: 1px solid color-mix(in srgb, var(--accent-blue) 30%, transparent);
    outline-offset: -1px;
  }

  .job-row.selected {
    background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-surface));
  }

  .job-row.member td {
    background: var(--bg-inset);
  }

  .job-row td {
    padding: 6px 10px;
    font-size: var(--font-size-sm);
    color: var(--text-primary);
    vertical-align: middle;
    white-space: nowrap;
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }

  .col-id {
    width: 60px;
    color: var(--text-muted);
    text-align: right;
    white-space: nowrap;
  }

  .chevron {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    width: 14px;
    height: 14px;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    transition:
      transform 0.1s,
      background 0.1s,
      color 0.1s;
    vertical-align: middle;
    margin-right: 2px;
    padding: 0;
    border: 0;
    background: transparent;
    cursor: pointer;
  }

  .chevron:hover {
    background: var(--bg-inset);
    color: var(--text-primary);
  }

  .chevron.open {
    transform: rotate(90deg);
    color: var(--accent-blue);
  }

  .col-ref {
    min-width: 160px;
    max-width: 300px;
    white-space: normal;
  }

  .ref-line {
    display: flex;
    align-items: flex-start;
    gap: 4px;
    min-width: 0;
  }

  .tree-cell .ref-line--member {
    padding-left: 18px;
  }

  .tree-spacer {
    flex: 0 0 14px;
    width: 14px;
    height: 14px;
  }

  .ref-stack {
    display: block;
    min-width: 0;
  }

  .ref-group {
    display: flex;
    align-items: center;
    gap: 4px;
    flex-wrap: wrap;
  }

  .repo-name {
    font-weight: 500;
    font-size: var(--font-size-sm);
  }

  .branch-name {
    color: var(--accent-purple);
    font-size: var(--font-size-xs);
  }

  .git-ref {
    color: var(--text-muted);
  }

  .commit-subject {
    display: block;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 280px;
  }

  .member-name {
    display: block;
    font-size: var(--font-size-xs);
    color: var(--accent-blue);
    font-weight: 500;
  }

  .panel-status {
    display: block;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
  }

  .col-agent {
    max-width: 100px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .col-status {
    width: 90px;
  }

  .col-verdict {
    width: 70px;
  }

  .col-elapsed {
    width: 80px;
    color: var(--text-secondary);
    text-align: right;
  }

  .col-cost {
    width: 72px;
    color: var(--text-secondary);
    text-align: right;
  }

  .col-type {
    width: 80px;
    color: var(--text-secondary);
  }

  .col-queued {
    width: 80px;
    color: var(--text-muted);
    text-align: right;
  }
</style>
