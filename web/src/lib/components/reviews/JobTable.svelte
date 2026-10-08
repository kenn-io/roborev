<script lang="ts">
  import { EmptyState, TableHeaderCell } from "@kenn-io/kit-ui";
  import { getReviewStores } from "../../stores/context";
  import type { SortColumn } from "../../stores/roborev/jobs.svelte";
  import { isPanelParent } from "../../utils/roborev-panel";
  import JobRow from "./JobRow.svelte";

  const stores = getReviewStores();
  const jobsStore = stores.roborevJobs;

  interface ColumnDef {
    key: SortColumn;
    label: string;
    numeric?: boolean;
  }

  const columns: ColumnDef[] = [
    { key: "id", label: "ID", numeric: true },
    { key: "repo", label: "Commit" },
    { key: "agent", label: "Agent" },
    { key: "reasoning", label: "Reasoning" },
    { key: "review_type", label: "Type" },
    { key: "status", label: "Status" },
    { key: "verdict", label: "Verdict" },
    { key: "closed", label: "Closed" },
    { key: "elapsed", label: "Elapsed", numeric: true },
    { key: "cost", label: "Cost", numeric: true },
    { key: "enqueued_at", label: "Queued", numeric: true },
  ];

  const sortLabels: Record<SortColumn, string> = {
    id: "ID",
    repo: "commit",
    closed: "closed state",
    status: "status",
    verdict: "verdict",
    agent: "agent",
    reasoning: "reasoning",
    review_type: "type",
    elapsed: "elapsed time",
    cost: "cost",
    enqueued_at: "queue time",
  };

  const partialSort = $derived(
    jobsStore !== undefined &&
      !jobsStore.areAllJobsLoaded() &&
      !(
        jobsStore.getSortColumn() === "enqueued_at" &&
        jobsStore.getSortDirection() === "desc"
      ),
  );

  function sortDirection(col: ColumnDef) {
    return jobsStore?.getSortColumn() === col.key
      ? jobsStore.getSortDirection()
      : null;
  }
</script>

<!-- Scrollable regions need keyboard access. -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<div class="table-wrapper" role="region" aria-label="Review jobs" tabindex="0">
  <table class="job-table">
    <thead>
      <tr>
        {#each columns as col (col.key)}
          <TableHeaderCell
            label={col.label}
            numeric={col.numeric ?? false}
            sortable
            sortDirection={sortDirection(col)}
            onsort={() => jobsStore?.setSortColumn(col.key)}
            class={`th-${col.key}`}
          />
        {/each}
      </tr>
    </thead>
    <tbody>
      {#if jobsStore}
        {#each jobsStore.getJobs() as job (job.id)}
          {@const runUuid = job.panel_run_uuid ?? undefined}
          {@const expandable = isPanelParent(job) && runUuid !== undefined}
          {@const expanded =
            expandable &&
            runUuid !== undefined &&
            jobsStore.isPanelExpanded(runUuid)}
          {@const members =
            runUuid !== undefined
              ? jobsStore.getPanelMembers(runUuid)
              : undefined}
          {@const memberError =
            runUuid !== undefined
              ? jobsStore.getPanelMemberError(runUuid)
              : undefined}
          <JobRow
            {job}
            {members}
            {expandable}
            {expanded}
            selected={jobsStore.getSelectedJobId() === job.id}
            highlighted={jobsStore.getHighlightedJobId() === job.id}
            onclick={() => jobsStore.selectJob(job.id)}
            ontoggle={() => jobsStore.togglePanel(job)}
          />
          {#if expanded && runUuid !== undefined}
            {#if jobsStore.isLoadingMembers(runUuid) && members === undefined}
              <tr class="members-status-row">
                <td colspan={columns.length}>Loading reviewers…</td>
              </tr>
            {:else}
              {#each members ?? [] as panelMember (panelMember.id)}
                <JobRow
                  job={panelMember}
                  member
                  selected={jobsStore.getSelectedJobId() === panelMember.id}
                  highlighted={jobsStore.getHighlightedJobId() ===
                    panelMember.id}
                  onclick={() => jobsStore.selectJob(panelMember.id)}
                />
              {/each}
              {#if jobsStore.isLoadingMembers(runUuid)}
                <tr class="members-status-row">
                  <td colspan={columns.length}>Refreshing reviewers…</td>
                </tr>
              {/if}
              {#if memberError}
                <tr class="members-status-row error">
                  <td colspan={columns.length}>
                    Could not refresh reviewers.
                    <button
                      type="button"
                      class="members-retry"
                      onclick={() => jobsStore.refreshPanelMembers(runUuid)}
                    >
                      Retry
                    </button>
                  </td>
                </tr>
              {/if}
            {/if}
          {/if}
        {/each}
      {/if}
    </tbody>
  </table>

  {#if jobsStore?.isLoading()}
    <div class="loading-bar">Loading...</div>
  {/if}

  {#if jobsStore?.getError()}
    <div class="error-bar">
      {jobsStore.getError()}
    </div>
  {/if}

  {#if jobsStore && !jobsStore.isLoading() && jobsStore.getJobs().length === 0}
    <EmptyState title="No jobs found" />
  {/if}

  {#if jobsStore?.getHasMore() || partialSort}
    <div class="table-footer">
      {#if partialSort && jobsStore}
        <span class="sort-scope" role="status">
          Sorted by {sortLabels[jobsStore.getSortColumn()]} across the
          {jobsStore.getJobs().length} most recent jobs. Load more to include older
          jobs.
        </span>
      {/if}
      {#if jobsStore?.getHasMore()}
        <button
          class="load-more-btn"
          disabled={jobsStore.isLoading()}
          onclick={() => jobsStore.loadMore()}
        >
          Load more
        </button>
      {/if}
    </div>
  {/if}
</div>

<style>
  /* Tables scroll both axes in narrow hosts (640px workspace sidebar), so
     this stays a native scroller instead of the vertical-only ScrollBox:
     hiding the native bars would drop the horizontal affordance, and a
     nested x-scroller would detach the sticky thead from the scrollport. */
  .table-wrapper {
    background: var(--bg-surface);
    overflow: auto;
    flex: 1;
    min-height: 0;
  }

  .job-table {
    width: 100%;
    border-collapse: collapse;
    table-layout: auto;
  }

  thead {
    position: sticky;
    top: 0;
    z-index: 1;
  }

  .job-table :global(.kit-th) {
    padding: 6px 10px;
    background: var(--bg-surface);
    color: var(--text-muted);
    font-weight: 500;
    box-shadow: inset 0 -1px 0 var(--border-default);
    border-bottom: 0;
    vertical-align: middle;
  }

  .job-table :global(.kit-th.th-id) {
    padding-left: 14px;
  }

  .loading-bar {
    padding: 12px;
    text-align: center;
    font-size: var(--font-size-sm);
    color: var(--text-muted);
  }

  .error-bar {
    padding: 12px;
    text-align: center;
    font-size: var(--font-size-sm);
    color: var(--accent-red);
  }

  .members-status-row td {
    padding: 6px 10px 6px 34px;
    font-size: var(--font-size-xs);
    color: var(--text-muted);
    background: var(--bg-inset);
  }

  .members-status-row.error td {
    color: var(--accent-red);
  }

  .members-retry {
    margin-left: 8px;
    border: 0;
    padding: 0;
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    text-decoration: underline;
    cursor: pointer;
  }

  .table-footer {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-6);
    padding: 10px 16px;
    border-top: 1px solid var(--border-muted);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .load-more-btn {
    padding: 4px 14px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 500;
    cursor: pointer;
    transition: background var(--transition-fast);
  }

  .load-more-btn:hover {
    background: var(--bg-surface-hover);
  }

  .load-more-btn:disabled {
    opacity: var(--opacity-disabled);
    cursor: default;
  }
</style>
