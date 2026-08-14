<script lang="ts">
  import { getReviewStores } from "../../stores/context";

  const stores = getReviewStores();
  const daemon = stores.roborevDaemon;

  const counts = $derived(stores.roborevJobs?.getFilteredStatusCounts());
  const filteredCounts = $derived(
    stores.roborevJobs?.usesFilteredStatusCounts() ?? false,
  );

  function displayCount(
    filtered: number | undefined,
    daemonTotal: number,
  ): number | string {
    return filteredCounts ? (filtered ?? "--") : daemonTotal;
  }

  function formatVersion(version: string): string {
    if (version.startsWith("v")) return version;
    return `v${version}`;
  }
</script>

{#if daemon}
  <div class="daemon-status">
    <span
      class="conn-indicator"
      class:connected={daemon.isAvailable()}
      title={daemon.isAvailable() ? "Connected" : "Disconnected"}
    >
      <span class="conn-dot"></span>
      {daemon.isAvailable() ? "Connected" : "Disconnected"}
    </span>

    {#if daemon.isAvailable()}
      <span class="separator"></span>

      <span class="status-item" title="Daemon version">
        {formatVersion(daemon.getVersion())}
      </span>

      <span class="separator"></span>

      <span class="status-item" title="Active / max workers">
        Workers {daemon.getActiveWorkers()}/{daemon.getMaxWorkers()}
      </span>

      <span class="separator"></span>

      <span class="status-counts">
        <span class="count count-queued" title="Queued">
          {displayCount(counts?.queued, daemon.getQueuedJobs())} queued
        </span>
        <span class="count count-running" title="Running">
          {displayCount(counts?.running, daemon.getRunningJobs())} running
        </span>
        <span class="count count-done" title="Done">
          {displayCount(counts?.done, daemon.getCompletedJobs())} done
        </span>
        <span class="count count-failed" title="Failed">
          {displayCount(counts?.failed, daemon.getFailedJobs())} failed
        </span>
      </span>
    {:else}
      <button class="retry-btn" onclick={() => daemon.checkHealth()}>
        Retry
      </button>
    {/if}
  </div>
{/if}

<style>
  .daemon-status {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 12px;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-surface);
    flex-shrink: 0;
  }

  .conn-indicator {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-weight: 500;
  }

  .conn-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--review-failed);
    flex-shrink: 0;
  }

  .connected .conn-dot {
    background: var(--review-done);
  }

  .separator {
    width: 1px;
    height: 12px;
    background: var(--border-muted);
    flex-shrink: 0;
  }

  .status-item {
    white-space: nowrap;
  }

  .status-counts {
    display: flex;
    gap: 8px;
  }

  .count {
    white-space: nowrap;
  }

  .count-queued {
    color: var(--review-queued);
  }
  .count-running {
    color: var(--review-running);
  }
  .count-done {
    color: var(--review-done);
  }
  .count-failed {
    color: var(--review-failed);
  }

  .retry-btn {
    padding: 2px 8px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    cursor: pointer;
  }

  .retry-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
</style>
