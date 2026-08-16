<script lang="ts">
  import { EmptyState } from "@kenn-io/kit-ui";
  import { getAppRuntime } from "../runtime/context";
  import { getReviewStores } from "../stores/context";
  import FilterBar from "../components/reviews/FilterBar.svelte";
  import DaemonStatus from "../components/reviews/DaemonStatus.svelte";
  import JobTable from "../components/reviews/JobTable.svelte";
  import ReviewDrawer from "../components/reviews/ReviewDrawer.svelte";
  import ShortcutHelpModal from "../components/reviews/ShortcutHelpModal.svelte";
  import {
    canCancelJob,
    canRerunJob,
    isPanelParent,
  } from "../utils/roborev-panel";

  interface Props {
    jobId?: number;
  }
  let { jobId }: Props = $props();

  const stores = getReviewStores();
  const runtime = getAppRuntime();

  let helpOpen = $state(false);
  let activeTab = $state<"review" | "log" | "prompt">("review");

  // Sync route jobId to store without navigating.
  // Using selectJob() here would call navigate(), which
  // updates the route, which passes a new jobId prop,
  // causing an infinite effect cycle.
  $effect(() => {
    if (stores.roborevJobs) {
      stores.roborevJobs.setSelectedJobId(jobId);
    }
  });

  // Sync selected job to review store
  $effect(() => {
    const id = stores.roborevJobs?.getSelectedJobId();
    stores.roborevReview?.setSelectedJobId(id);
  });

  // Reset to review tab when drawer opens
  $effect(() => {
    const id = stores.roborevJobs?.getSelectedJobId();
    if (id !== undefined) {
      activeTab = "review";
    }
  });

  function handleKeydown(e: KeyboardEvent): void {
    if (!(e.target instanceof HTMLElement)) return;
    const tag = e.target.tagName;
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") {
      return;
    }

    if (e.metaKey || e.ctrlKey || e.altKey) return;

    const daemonDown = !stores.roborevDaemon?.isAvailable();

    if (helpOpen) {
      if (e.key === "Escape" || e.key === "?") {
        e.preventDefault();
        helpOpen = false;
      }
      return;
    }

    const drawerOpen = stores.roborevJobs?.getSelectedJobId() !== undefined;

    switch (e.key) {
      case "j":
        e.preventDefault();
        stores.roborevJobs?.highlightNextJob();
        break;
      case "k":
        e.preventDefault();
        stores.roborevJobs?.highlightPrevJob();
        break;
      case "ArrowRight":
      case "ArrowLeft": {
        const jobsStore = stores.roborevJobs;
        if (!jobsStore) break;
        const highlightedId = jobsStore.getHighlightedJobId();
        const highlighted = jobsStore
          .getVisibleJobs()
          .find((candidate) => candidate.id === highlightedId);
        const panelParent =
          highlighted && isPanelParent(highlighted)
            ? highlighted
            : jobsStore
                .getJobs()
                .find(
                  (candidate) =>
                    isPanelParent(candidate) &&
                    candidate.panel_run_uuid === highlighted?.panel_run_uuid,
                );
        if (panelParent && panelParent.panel_run_uuid) {
          const open = jobsStore.isPanelExpanded(panelParent.panel_run_uuid);
          if (
            (e.key === "ArrowRight" && !open) ||
            (e.key === "ArrowLeft" && open)
          ) {
            e.preventDefault();
            jobsStore.togglePanel(panelParent);
          }
        }
        break;
      }
      case "Enter": {
        const highlighted = stores.roborevJobs?.getHighlightedJobId();
        if (highlighted !== undefined && !drawerOpen) {
          e.preventDefault();
          stores.roborevJobs?.selectJob(highlighted);
        }
        break;
      }
      case "Escape":
        if (drawerOpen) {
          e.preventDefault();
          stores.roborevJobs?.deselectJob();
        }
        break;
      case "x": {
        const jobsStore = stores.roborevJobs;
        const xId = jobsStore?.getSelectedJobId();
        const selected =
          jobsStore?.getVisibleJobs().find((job) => job.id === xId) ??
          stores.roborevReview?.getSelectedJob();
        if (
          jobsStore &&
          xId !== undefined &&
          selected &&
          canCancelJob(selected, stores.getCapabilities())
        ) {
          e.preventDefault();
          jobsStore.cancelJob(xId);
        }
        break;
      }
      case "r": {
        const jobsStore = stores.roborevJobs;
        const rId = jobsStore?.getSelectedJobId();
        const selected =
          jobsStore?.getVisibleJobs().find((job) => job.id === rId) ??
          stores.roborevReview?.getSelectedJob();
        if (
          jobsStore &&
          rId !== undefined &&
          selected &&
          stores.getCapabilities().rerunJob &&
          canRerunJob(selected) &&
          !jobsStore.isRerunning(rId)
        ) {
          e.preventDefault();
          jobsStore.rerunJob(rId);
        }
        break;
      }
      case "a":
        if (drawerOpen) {
          const aId = stores.roborevJobs?.getSelectedJobId();
          if (aId !== undefined) {
            e.preventDefault();
            stores.roborevReview?.closeReview(aId);
          }
        }
        break;
      case "c":
        if (drawerOpen) {
          e.preventDefault();
          const textarea = document.querySelector<HTMLElement>(
            ".comment-input textarea",
          );
          textarea?.focus();
        }
        break;
      case "l":
        if (drawerOpen) {
          e.preventDefault();
          activeTab = "log";
        }
        break;
      case "p":
        if (drawerOpen) {
          e.preventDefault();
          activeTab = "prompt";
        }
        break;
      case "y":
        if (drawerOpen) {
          e.preventDefault();
          stores.roborevReview?.copyOutput();
        }
        break;
      case "h":
        if (!drawerOpen && !daemonDown) {
          e.preventDefault();
          const cur = stores.roborevJobs?.getFilterHideClosed() ?? false;
          stores.roborevJobs?.setFilter("hideClosed", !cur);
        }
        break;
      case "/":
        if (e.shiftKey) {
          e.preventDefault();
          helpOpen = !helpOpen;
          break;
        }
        if (!drawerOpen && !daemonDown) {
          e.preventDefault();
          const searchInput = document.querySelector<HTMLElement>(
            ".filter-bar .kit-search-input input",
          );
          searchInput?.focus();
        }
        break;
      case "?":
        e.preventDefault();
        helpOpen = !helpOpen;
        break;
    }
  }

  $effect(() => {
    window.addEventListener("keydown", handleKeydown);
    return () => window.removeEventListener("keydown", handleKeydown);
  });

  $effect(() => {
    const jobs = stores.roborevJobs;
    if (!jobs || !stores.roborevDaemon?.isAvailable()) return;
    const execution = runtime.runCommand(jobs.loadJobsEffect(), {
      operation: "hydrate mounted Roborev jobs",
      safeContext: {},
      onFailure: () => {},
    });
    return execution.interrupt;
  });

  $effect(() => {
    const review = stores.roborevReview;
    const jobs = stores.roborevJobs;
    const selectedId = jobs?.getSelectedJobId();
    const revision = jobs?.getSelectedReviewRevision();
    if (
      !review ||
      selectedId === undefined ||
      !stores.roborevDaemon?.isAvailable()
    )
      return;
    void revision;
    const execution = runtime.runCommand(review.loadReviewEffect(selectedId), {
      operation: "hydrate mounted Roborev review",
      safeContext: { jobId: selectedId },
      onFailure: () => {},
    });
    return execution.interrupt;
  });

  $effect(() => {
    const jobsStore = stores.roborevJobs;
    if (!jobsStore || !stores.roborevDaemon?.isAvailable()) {
      return;
    }
    const eventOwner = jobsStore.connectEventStream("/");

    return () => {
      jobsStore.disconnectEventStream(eventOwner);
    };
  });
</script>

<div class="reviews-view">
  {#if !stores.roborevDaemon}
    <EmptyState title="Roborev integration is not configured." />
  {:else if !stores.roborevDaemon.isAvailable() && !stores.roborevDaemon.getWasEverAvailable()}
    <EmptyState
      title="Roborev daemon not reachable"
      description={stores.roborevDaemon.getEndpoint()}
    >
      <button
        class="retry-btn"
        onclick={() => stores.roborevDaemon?.checkHealth()}
      >
        Retry
      </button>
    </EmptyState>
  {:else}
    <div class="reviews-header">
      <FilterBar
        onHelpClick={() => (helpOpen = true)}
        disabled={!stores.roborevDaemon.isAvailable()}
      />
      <DaemonStatus />
    </div>
    <div class="reviews-body">
      <div class="reviews-table">
        <JobTable />
      </div>
      <ReviewDrawer bind:activeTab />
    </div>
  {/if}
</div>

<ShortcutHelpModal open={helpOpen} onclose={() => (helpOpen = false)} />

<style>
  .reviews-view {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;
    overflow: hidden;
  }

  .reviews-header {
    flex-shrink: 0;
  }

  .reviews-body {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;
    overflow: hidden;
  }

  .reviews-table {
    flex: 1;
    min-height: 0;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  .retry-btn {
    padding: 6px 16px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-default);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-md);
    cursor: pointer;
  }

  .retry-btn:hover {
    background: var(--bg-surface-hover);
  }
</style>
