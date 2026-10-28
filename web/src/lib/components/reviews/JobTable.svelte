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
    { key: "review_type", label: "Type" },
    { key: "status", label: "Status" },
    { key: "verdict", label: "Verdict" },
    { key: "closed", label: "Closed" },
    { key: "elapsed", label: "Elapsed", numeric: true },
    { key: "cost", label: "Cost", numeric: true },
    { key: "enqueued_at", label: "Queued", numeric: true },
  ];

  let scroller = $state<HTMLDivElement>();
  let sentinel = $state<HTMLDivElement>();

  const canLoadMore = $derived(
    jobsStore !== undefined &&
      jobsStore.getHasMore() &&
      jobsStore.getJobs().length > 0 &&
      !jobsStore.isLoading() &&
      jobsStore.getError() === null &&
      jobsStore.getLoadMoreError() === null,
  );

  const hitJobLimit = $derived(
    jobsStore !== undefined &&
      !jobsStore.getHasMore() &&
      !jobsStore.areAllJobsLoaded(),
  );

  // A new observer reports the sentinel's position once on observe(). The
  // effect builds one each time a load finishes, so a page that leaves the
  // sentinel in view loads the next page until the table fills the scroll
  // container or the daemon has no more jobs. Disconnecting before
  // loadMore() keeps a second intersection from replacing the in-flight
  // request.
  $effect(() => {
    if (!canLoadMore || !scroller || !sentinel) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (!entries.some((entry) => entry.isIntersecting)) return;
        observer.disconnect();
        jobsStore.loadMore();
      },
      { root: scroller, rootMargin: "0px 0px 200px 0px" },
    );
    observer.observe(sentinel);
    return () => observer.disconnect();
  });

  function sortDirection(col: ColumnDef) {
    return jobsStore?.getSortColumn() === col.key
      ? jobsStore.getSortDirection()
      : null;
  }
</script>

<!-- Scrollable regions need keyboard access. -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<div
  class="table-wrapper"
  role="region"
  aria-label="Review jobs"
  tabindex="0"
  bind:this={scroller}
>
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

  {#if jobsStore?.getHasMore()}
    <div class="scroll-sentinel" aria-hidden="true" bind:this={sentinel}></div>
  {/if}

  {#if jobsStore?.isLoading()}
    <div class="loading-bar" role="status">Loading...</div>
  {/if}

  {#if jobsStore?.getError()}
    <div class="error-bar">
      {jobsStore.getError()}
    </div>
  {/if}

  {#if jobsStore && !jobsStore.isLoading() && jobsStore.getJobs().length === 0}
    <EmptyState title="No jobs found" />
  {/if}

  {#if jobsStore && jobsStore.getLoadMoreError() !== null}
    <div class="error-bar" role="alert">
      Could not load more jobs.
      <button
        type="button"
        class="load-more-retry"
        onclick={() => jobsStore.loadMore()}
      >
        Retry
      </button>
    </div>
  {/if}

  {#if hitJobLimit && jobsStore}
    <div class="loading-bar">
      Showing the first {jobsStore.getJobs().length.toLocaleString()} jobs in this
      order, the most the table loads. Filter the list to reach the rest.
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

  /* Sticky so the sentinel stays inside the observed area when a wide table
     is scrolled right by more than the wrapper's width. */
  .scroll-sentinel {
    position: sticky;
    left: 0;
    height: 1px;
  }

  .members-retry,
  .load-more-retry {
    margin-left: 8px;
    border: 0;
    padding: 0;
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    text-decoration: underline;
    cursor: pointer;
  }
</style>
