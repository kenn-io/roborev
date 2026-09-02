<script lang="ts">
  import { IconButton, TopBar, type TopBarTab } from "@kenn-io/kit-ui";
  import BookOpenIcon from "@lucide/svelte/icons/book-open";
  import { Effect } from "effect";
  import { onDestroy } from "svelte";

  import { appPath } from "../base-path";
  import { createRoborevClient, executeRoborevRequest } from "../api/client";
  import type { components } from "../api/generated";
  import type { SessionCapabilities } from "../api/session";
  import { createRouter } from "../router/router.svelte";
  import { setAppRuntime, setRoborevClient } from "../runtime/context";
  import { makeAppRuntime } from "../runtime/runtime";
  import { createReviewStores } from "../stores/composition.svelte";
  import { provideReviewStores } from "../stores/context";
  import AnalyticsView from "../views/AnalyticsView.svelte";
  import ReviewsView from "../views/ReviewsView.svelte";
  import ReleaseNotesModal from "./ReleaseNotesModal.svelte";

  type ReleaseNote = components["schemas"]["ReleaseNote"];

  interface Props {
    capabilities: SessionCapabilities;
  }
  let { capabilities }: Props = $props();

  const runtime = makeAppRuntime();
  const client = createRoborevClient(appPath("/"));
  const router = createRouter();
  const route = $derived(router.getRoute());
  const navigationTabs: TopBarTab[] = [
    { id: "reviews", label: "Reviews" },
    { id: "analytics", label: "Analytics" },
  ];
  let actionError = $state<string | null>(null);
  let releaseNotesOpen = $state(false);
  let releaseNotes = $state<ReadonlyArray<ReleaseNote>>([]);
  let releaseNotesLoading = $state(false);
  let releaseNotesStale = $state(false);
  let releaseNotesError = $state<string | null>(null);
  let releaseNotesRequest: ReturnType<typeof runtime.runCommand> | undefined;
  const stores = createReviewStores({
    runtime,
    client,
    navigate: router.navigateToReview,
    getCapabilities: () => capabilities,
    daemonInitiallyAvailable: true,
    onError: (message) => {
      actionError = message;
    },
  });
  setAppRuntime(runtime);
  setRoborevClient(client);
  provideReviewStores(stores);

  const polling = runtime.runCommand(stores.roborevDaemon.pollingEffect, {
    operation: "poll Roborev daemon status",
    safeContext: {},
    onFailure: () => {},
  });

  onDestroy(() => {
    polling.interrupt();
    releaseNotesRequest?.interrupt();
    stores.roborevJobs.dispose();
    router.dispose();
    void Effect.runPromise(runtime.disposeEffect);
  });

  function navigateReviews(event: MouseEvent): void {
    if (!shouldHandleNavigation(event)) return;
    event.preventDefault();
    router.navigateToReview();
  }

  function navigatePage(page: string): void {
    if (page === "analytics") {
      router.navigateToAnalytics();
      return;
    }
    router.navigateToReview();
  }

  function shouldHandleNavigation(event: MouseEvent): boolean {
    return (
      event.button === 0 &&
      !event.metaKey &&
      !event.ctrlKey &&
      !event.shiftKey &&
      !event.altKey
    );
  }

  function openReleaseNotes(): void {
    releaseNotesOpen = true;
    if (!releaseNotesLoading) loadReleaseNotes();
  }

  function loadReleaseNotes(): void {
    releaseNotesRequest?.interrupt();
    releaseNotesLoading = true;
    releaseNotesError = null;
    releaseNotesRequest = runtime.runCommand(
      executeRoborevRequest("list Roborev releases", (signal) =>
        client.GET("/api/releases", { signal }),
      ).pipe(
        Effect.tap((result) =>
          Effect.sync(() => {
            if (result.data) {
              releaseNotes = result.data.releases ?? [];
              releaseNotesStale = result.data.stale;
              return;
            }
            releaseNotesError = "The daemon could not retrieve release notes.";
          }),
        ),
        Effect.ensuring(
          Effect.sync(() => {
            releaseNotesLoading = false;
          }),
        ),
      ),
      {
        operation: "list Roborev releases",
        safeContext: {},
        onFailure: () => {
          releaseNotesError = "The daemon could not retrieve release notes.";
        },
      },
    );
  }
</script>

<div class="app-shell">
  <TopBar
    tabs={navigationTabs}
    active={route.page}
    onchange={navigatePage}
    ariaLabel="Application"
  >
    {#snippet left()}
      <a class="brand" href={appPath("/reviews")} onclick={navigateReviews}
        >Roborev</a
      >
    {/snippet}
    {#snippet right()}
      <IconButton ariaLabel="Release notes" onclick={openReleaseNotes}>
        <BookOpenIcon size="16" strokeWidth="2" aria-hidden="true" />
      </IconButton>
    {/snippet}
  </TopBar>

  <div class="app-content">
    {#if route.page === "analytics"}
      <AnalyticsView />
    {:else}
      <ReviewsView jobId={route.jobId} />
    {/if}
  </div>
  {#if actionError}
    <div class="action-error" role="alert">
      <span>{actionError}</span>
      <button
        type="button"
        aria-label="Dismiss error"
        onclick={() => (actionError = null)}>Dismiss</button
      >
    </div>
  {/if}
  <ReleaseNotesModal
    open={releaseNotesOpen}
    releases={releaseNotes}
    loading={releaseNotesLoading}
    stale={releaseNotesStale}
    error={releaseNotesError}
    onclose={() => (releaseNotesOpen = false)}
    onretry={loadReleaseNotes}
  />
</div>

<style>
  .app-shell {
    display: flex;
    width: 100%;
    min-height: 0;
    min-width: 0;
    flex: 1;
    flex-direction: column;
    background: var(--bg-primary);
    color: var(--text-primary);
  }

  .brand {
    display: inline-flex;
    align-items: center;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 700;
    letter-spacing: -0.01em;
    text-decoration: none;
    white-space: nowrap;
  }

  .app-content {
    display: flex;
    min-height: 0;
    min-width: 0;
    flex: 1;
  }

  .action-error {
    position: fixed;
    right: 16px;
    bottom: 16px;
    z-index: 100;
    display: flex;
    max-width: min(420px, calc(100vw - 32px));
    align-items: center;
    gap: 12px;
    padding: 10px 12px;
    border: 1px solid var(--accent-red);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    box-shadow: var(--shadow-md);
    color: var(--text-primary);
  }

  .action-error button {
    border: 0;
    background: transparent;
    color: var(--text-secondary);
    cursor: pointer;
  }
</style>
