<script lang="ts">
  import { BottomDock, Button, FitStages, IconButton } from "@kenn-io/kit-ui";
  import BanIcon from "@lucide/svelte/icons/ban";
  import CircleCheckIcon from "@lucide/svelte/icons/circle-check";
  import CopyIcon from "@lucide/svelte/icons/copy";
  import RefreshIcon from "@lucide/svelte/icons/refresh-cw";
  import Undo2Icon from "@lucide/svelte/icons/undo-2";
  import { getReviewStores } from "../../stores/context";
  import StatusBadge from "./StatusBadge.svelte";
  import VerdictBadge from "./VerdictBadge.svelte";
  import ReviewContent from "./ReviewContent.svelte";
  import ResponseList from "./ResponseList.svelte";
  import LogViewer from "./LogViewer.svelte";
  import PromptViewer from "./PromptViewer.svelte";
  import {
    canCancelJob,
    canRerunJob,
    isPanelParent,
    isTerminalStatus,
    panelReviewHeader,
  } from "../../utils/roborev-panel";
  import {
    parseTokenUsage,
    tokenUsageDetail,
    tokenUsageStats,
  } from "../../utils/roborev-usage";

  interface Props {
    activeTab?: "review" | "log" | "prompt";
  }
  let { activeTab = $bindable("review") }: Props = $props();

  const stores = getReviewStores();

  // Prefer the live table row (updated by SSE/mutations).
  // Fall back to the review store's fetched job for
  // off-page deep links where the job isn't in the table.
  const selectedJob = $derived(
    stores.roborevJobs
      ?.getVisibleJobs()
      .find((j) => j.id === stores.roborevJobs?.getSelectedJobId()) ??
      stores.roborevReview?.getSelectedJob(),
  );

  const isOpen = $derived(stores.roborevJobs?.getSelectedJobId() !== undefined);

  function close(): void {
    stores.roborevJobs?.deselectJob();
  }

  function shortRef(ref: string): string {
    if (ref.length > 10) return ref.slice(0, 8);
    return ref;
  }

  function copyOutput(): void {
    stores.roborevReview?.copyOutput();
  }

  function handleCloseReview(): void {
    const jobId = stores.roborevJobs?.getSelectedJobId();
    if (jobId !== undefined) {
      stores.roborevReview?.closeReview(jobId);
    }
  }

  function handleRerun(): void {
    const jobId = stores.roborevJobs?.getSelectedJobId();
    if (jobId !== undefined) {
      stores.roborevJobs?.rerunJob(jobId);
    }
  }

  function handleCancel(): void {
    const jobId = stores.roborevJobs?.getSelectedJobId();
    if (jobId !== undefined) {
      stores.roborevJobs?.cancelJob(jobId);
    }
  }

  const rerunPending = $derived(
    selectedJob
      ? (stores.roborevJobs?.isRerunning(selectedJob.id) ?? false)
      : false,
  );

  const hasReview = $derived(stores.roborevReview?.getReview() != null);

  const reviewIsClosed = $derived(stores.roborevReview?.isClosed() ?? false);

  // Shared by both footer stages so the accessible name of an action never
  // depends on how much room the drawer happens to have.
  const closeLabel = $derived(reviewIsClosed ? "Reopen" : "Close Review");
  const closeTitle = $derived(
    reviewIsClosed ? "Reopen review" : "Close review",
  );

  const tokenUsage = $derived(parseTokenUsage(selectedJob?.token_usage));

  let interestedPanelRun: string | undefined;

  const selectedPanelRun = $derived(
    selectedJob && isPanelParent(selectedJob)
      ? selectedJob.panel_run_uuid
      : undefined,
  );

  const panelError = $derived(
    selectedPanelRun
      ? stores.roborevJobs?.getPanelMemberError(selectedPanelRun)
      : undefined,
  );

  const panelLoading = $derived(
    selectedPanelRun
      ? (stores.roborevJobs?.isLoadingMembers(selectedPanelRun) ?? false)
      : false,
  );

  const panelMembers = $derived.by(() => {
    const runUuid = selectedJob?.panel_run_uuid;
    if (!runUuid) return undefined;
    return stores.roborevJobs?.getPanelMembers(runUuid);
  });

  const canCancel = $derived(
    selectedJob ? canCancelJob(selectedJob, stores.getCapabilities()) : false,
  );

  const canRerun = $derived(
    stores.getCapabilities().rerunJob && selectedJob
      ? canRerunJob(selectedJob)
      : false,
  );

  const panelHeader = $derived(
    selectedJob ? panelReviewHeader(selectedJob, panelMembers) : null,
  );

  $effect(() => {
    if (interestedPanelRun === selectedPanelRun) return;
    interestedPanelRun = selectedPanelRun;
    stores.roborevJobs?.setPanelMemberInterest(selectedPanelRun);
  });
</script>

<!-- kit-ui-check-ignore: this is the shared BottomDock primitive; the review-drawer class only preserves the feature's styling hook -->
<BottomDock
  class="review-drawer"
  open={isOpen}
  onclose={close}
  ariaLabel="Review details"
  initialHeight="50vh"
  minHeight="200px"
  maxHeight="80vh"
  closeTitle="Close review details"
  closeAriaLabel="Close review details"
>
  {#snippet header()}
    <div class="review-dock-header review-header-content">
      <div class="header-start">
        {#if selectedJob}
          <span class="job-id">
            #{selectedJob.id}
          </span>
          <VerdictBadge verdict={selectedJob.verdict} />
          <span class="header-meta">
            {#if selectedJob.repo_name}
              <span class="repo-name">
                {selectedJob.repo_name}
              </span>
            {/if}
            {#if selectedJob.branch}
              <span class="branch">
                {selectedJob.branch}
              </span>
            {/if}
            <span class="git-ref" title={selectedJob.git_ref}>
              {shortRef(selectedJob.git_ref)}
            </span>
          </span>
          <span class="header-agent">
            {selectedJob.agent}
            {#if selectedJob.model}
              / {selectedJob.model}
            {/if}
          </span>
          {#if selectedJob.review_type}
            <span class="review-type">
              {selectedJob.review_type}
            </span>
          {/if}
          {#if selectedJob.source === "auto_design"}
            <span class="review-type"> auto-design </span>
          {/if}
          {#if selectedJob.panel_member_name}
            <span class="review-type">
              {selectedJob.panel_member_name}
            </span>
          {/if}
          <StatusBadge status={selectedJob.status} />
        {/if}
      </div>

      {#if panelHeader}
        <div class="panel-line">
          {panelHeader}
          {#if panelError}
            <span class="panel-error"> Could not refresh reviewers. </span>
            {#if selectedPanelRun}
              <button
                type="button"
                class="panel-retry"
                onclick={() =>
                  stores.roborevJobs?.refreshPanelMembers(selectedPanelRun)}
              >
                Retry
              </button>
            {/if}
          {:else if panelLoading}
            <span class="panel-progress"> Refreshing reviewers... </span>
          {:else if selectedJob && !isTerminalStatus(selectedJob.status)}
            <span class="panel-progress"> Panel still synthesizing... </span>
          {/if}
        </div>
      {/if}

      <div class="tab-bar">
        <button
          class="tab"
          class:active={activeTab === "review"}
          onclick={() => (activeTab = "review")}
        >
          Review
        </button>
        <button
          class="tab"
          class:active={activeTab === "log"}
          onclick={() => (activeTab = "log")}
        >
          Log
        </button>
        <button
          class="tab"
          class:active={activeTab === "prompt"}
          onclick={() => (activeTab = "prompt")}
        >
          Prompt
        </button>
      </div>
    </div>
  {/snippet}

  {#if activeTab === "review"}
    {#if selectedJob?.status === "skipped" && selectedJob.skip_reason}
      <div class="skip-reason">Skipped: {selectedJob.skip_reason}</div>
    {/if}
    <ReviewContent />
    <div class="responses-section">
      <ResponseList />
    </div>
  {:else if activeTab === "log"}
    {#if selectedJob}
      <LogViewer jobId={selectedJob.id} jobStatus={selectedJob.status} />
    {/if}
  {:else if activeTab === "prompt"}
    <PromptViewer />
  {/if}

  {#snippet footer()}
    {#snippet labelledActions()}
      <div class="footer-actions" role="group" aria-label="Review actions">
        {#if hasReview}
          <Button
            size="sm"
            onclick={handleCloseReview}
            title={closeTitle}
            label={closeLabel}
          />
        {/if}
        {#if canRerun}
          <Button
            size="sm"
            onclick={handleRerun}
            title={rerunPending ? "Rerun in progress" : "Rerun this job"}
            label={rerunPending ? "Rerunning…" : "Rerun"}
            disabled={rerunPending}
          />
        {/if}
        {#if canCancel}
          <Button
            size="sm"
            tone="danger"
            onclick={handleCancel}
            title="Cancel this job"
            label="Cancel"
          />
        {/if}
        <Button
          size="sm"
          onclick={copyOutput}
          title="Copy review output"
          label="Copy Output"
        />
      </div>
    {/snippet}
    {#snippet iconActions()}
      <div class="footer-actions" role="group" aria-label="Review actions">
        {#if hasReview}
          <IconButton
            size="sm"
            ariaLabel={closeLabel}
            title={closeTitle}
            onclick={handleCloseReview}
          >
            {#if reviewIsClosed}
              <Undo2Icon size={14} aria-hidden="true" />
            {:else}
              <CircleCheckIcon size={14} aria-hidden="true" />
            {/if}
          </IconButton>
        {/if}
        {#if canRerun}
          <IconButton
            size="sm"
            ariaLabel={rerunPending ? "Rerunning" : "Rerun"}
            title={rerunPending ? "Rerun in progress" : "Rerun this job"}
            onclick={handleRerun}
            disabled={rerunPending}
          >
            <RefreshIcon size={14} aria-hidden="true" />
          </IconButton>
        {/if}
        {#if canCancel}
          <IconButton
            size="sm"
            tone="danger"
            ariaLabel="Cancel"
            title="Cancel this job"
            onclick={handleCancel}
          >
            <BanIcon size={14} aria-hidden="true" />
          </IconButton>
        {/if}
        <IconButton
          size="sm"
          ariaLabel="Copy Output"
          title="Copy review output"
          onclick={copyOutput}
        >
          <CopyIcon size={14} aria-hidden="true" />
        </IconButton>
      </div>
    {/snippet}
    <div class="review-dock-footer">
      <FitStages
        class="footer-actions-fit"
        stages={[labelledActions, iconActions]}
      />
      {#if tokenUsage}
        <span class="token-usage" title={tokenUsageDetail(tokenUsage)}>
          {#each tokenUsageStats(tokenUsage) as stat (stat.label)}
            <span class="usage-stat">
              <span class="usage-label">{stat.label}</span>
              <span class="usage-value">{stat.value}</span>
            </span>
          {/each}
        </span>
      {/if}
    </div>
  {/snippet}
</BottomDock>

<style>
  :global(.review-drawer) {
    border-top: 2px solid var(--accent-blue);
  }

  .review-header-content {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .header-start {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    overflow: hidden;
    min-width: 0;
  }

  .job-id {
    font-size: var(--font-size-md);
    font-weight: 600;
    color: var(--text-primary);
    white-space: nowrap;
  }

  .header-meta {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: var(--font-size-sm);
  }

  .repo-name {
    font-weight: 500;
    color: var(--text-primary);
  }

  .branch {
    color: var(--accent-blue);
  }

  .git-ref {
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }

  .header-agent {
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .review-type {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
    padding: 1px 6px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    white-space: nowrap;
  }

  .panel-line {
    margin-top: 4px;
    padding: 4px;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    border-top: 1px solid var(--border-muted);
    display: flex;
    gap: 12px;
  }

  .panel-progress {
    color: var(--accent-amber);
  }

  .panel-error {
    color: var(--accent-red);
  }

  .panel-retry {
    border: 0;
    padding: 0;
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    text-decoration: underline;
    cursor: pointer;
  }

  .skip-reason {
    padding: 8px 12px;
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }

  .tab-bar {
    display: flex;
    gap: 0;
    border-top: 1px solid var(--border-muted);
    flex-shrink: 0;
    margin-top: 4px;
  }

  .tab {
    padding: 6px 14px;
    border: none;
    border-bottom: 2px solid transparent;
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: 500;
    cursor: pointer;
    margin-bottom: -1px;
  }

  .tab:hover {
    color: var(--text-primary);
  }

  .tab.active {
    color: var(--accent-blue);
    border-bottom-color: var(--accent-blue);
  }

  .responses-section {
    padding: 12px 20px 16px;
    border-top: 1px solid var(--border-muted);
  }

  .review-dock-footer {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    width: 100%;
    flex-wrap: wrap;
  }

  /* The actions stay one horizontal group; when the footer runs out of room
   * the usage summary wraps below instead of stacking the buttons, and past
   * that the group downgrades to icon-only rather than spilling out of the
   * drawer. FitStages owns that swap by measurement, so its host must be
   * sized by the footer and never by the stage it currently renders -- hence
   * a zero flex basis. The floor is the icon row's own intrinsic width (four
   * 24px controls plus their 8px gaps): declaring it makes a long usage
   * summary wrap to a second line instead of squeezing the actions narrower
   * than the most compact stage can draw. */
  .review-dock-footer :global(.footer-actions-fit) {
    flex: 1 1 0;
    min-width: 120px;
  }

  .footer-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  .token-usage {
    display: flex;
    align-items: center;
    gap: 12px;
    min-width: 0;
    overflow: hidden;
    font-size: var(--font-size-xs);
    color: var(--text-muted);
    white-space: nowrap;
  }

  .usage-stat {
    display: inline-flex;
    align-items: baseline;
    gap: 4px;
  }

  .usage-label {
    color: var(--text-muted);
  }

  .usage-value {
    font-family: var(--font-mono);
    color: var(--text-secondary);
  }
</style>
