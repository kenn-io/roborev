<script lang="ts">
  import { Button, Modal } from "@kenn-io/kit-ui";
  import type { Component } from "svelte";

  import type { components } from "../api/generated";
  import { pushModalFrame } from "../keyboard/modal-stack.svelte";
  import { untrack } from "svelte";

  type ReleaseNote = components["schemas"]["ReleaseNote"];
  type ReleaseContentComponent = Component<{
    output: string;
    emptyMessage?: string;
  }>;

  interface Props {
    open: boolean;
    releases: ReadonlyArray<ReleaseNote>;
    loading: boolean;
    stale: boolean;
    error: string | null;
    onclose: () => void;
    onretry: () => void;
  }

  let { open, releases, loading, stale, error, onclose, onretry }: Props =
    $props();
  let selectedTag = $state("");
  let ReleaseContent = $state<ReleaseContentComponent>();
  let rendererError = $state(false);
  let rendererPromise: Promise<void> | undefined;
  const selectedRelease = $derived(
    releases.find((release) => release.tag_name === selectedTag) ?? releases[0],
  );

  $effect(() => {
    if (!open) return;
    return untrack(() => pushModalFrame("roborev-release-notes", []));
  });

  $effect(() => {
    if (open && !ReleaseContent) void loadRenderer();
  });

  $effect(() => {
    if (releases.length === 0) {
      selectedTag = "";
      return;
    }
    if (!releases.some((release) => release.tag_name === selectedTag)) {
      selectedTag = releases[0].tag_name;
    }
  });

  function releaseDate(value: string): string {
    return new Intl.DateTimeFormat(undefined, {
      year: "numeric",
      month: "short",
      day: "numeric",
    }).format(new Date(value));
  }

  async function loadRenderer(): Promise<void> {
    if (ReleaseContent) return;
    if (rendererPromise) return rendererPromise;
    rendererError = false;
    rendererPromise = import("@kenn-io/roborev-ui/review-content")
      .then((module) => {
        ReleaseContent = module.default;
      })
      .catch(() => {
        rendererError = true;
      })
      .finally(() => {
        rendererPromise = undefined;
      });
    return rendererPromise;
  }
</script>

{#if open}
  <Modal
    title="Roborev release notes"
    {onclose}
    width="min(960px, calc(100vw - 32px))"
    maxWidth="min(960px, calc(100vw - 32px))"
  >
    {#if loading && releases.length === 0}
      <div class="release-state" aria-live="polite">
        Loading release notes...
      </div>
    {:else if error && releases.length === 0}
      <div class="release-state release-error" role="alert">
        <strong>Could not load release notes</strong>
        <p>{error}</p>
        <Button onclick={onretry}>Try again</Button>
      </div>
    {:else if releases.length === 0}
      <div class="release-state">No published releases found.</div>
    {:else}
      <div class="release-layout">
        <nav class="release-list" aria-label="Recent releases">
          {#each releases as release (release.tag_name)}
            <button
              type="button"
              class:active={release.tag_name === selectedRelease?.tag_name}
              aria-current={release.tag_name === selectedRelease?.tag_name
                ? "true"
                : undefined}
              onclick={() => (selectedTag = release.tag_name)}
            >
              <span>{release.name}</span>
              <small>
                {release.tag_name} · {releaseDate(release.published_at)}
              </small>
            </button>
          {/each}
        </nav>

        {#if selectedRelease}
          <article class="release-detail">
            <header>
              <div>
                <h2>{selectedRelease.name}</h2>
                <p>
                  {selectedRelease.tag_name} · {releaseDate(
                    selectedRelease.published_at,
                  )}
                  {selectedRelease.prerelease ? " · prerelease" : ""}
                </p>
              </div>
              <a
                href={selectedRelease.html_url}
                target="_blank"
                rel="noreferrer">View on GitHub</a
              >
            </header>
            <div class="release-copy">
              {#if ReleaseContent}
                <ReleaseContent
                  output={selectedRelease.body}
                  emptyMessage="No release notes were provided."
                />
              {:else if rendererError}
                <div class="release-state release-error" role="alert">
                  <strong>Could not render release notes</strong>
                  <Button onclick={() => void loadRenderer()}>Try again</Button>
                </div>
              {:else}
                <div class="release-state" aria-live="polite">
                  Rendering release notes...
                </div>
              {/if}
            </div>
          </article>
        {/if}
      </div>
      {#if stale}
        <p class="stale-note" role="status">
          GitHub could not be reached. These notes came from the local cache.
        </p>
      {/if}
    {/if}
  </Modal>
{/if}

<style>
  .release-layout {
    display: grid;
    min-height: min(580px, calc(100vh - 160px));
    grid-template-columns: minmax(180px, 240px) minmax(0, 1fr);
  }

  .release-list {
    overflow-y: auto;
    padding: var(--space-2);
    border-right: var(--border-width) solid var(--border-default);
    background: var(--bg-inset);
  }

  .release-list button {
    display: flex;
    width: 100%;
    flex-direction: column;
    gap: 3px;
    padding: 10px 12px;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-primary);
    cursor: pointer;
    text-align: left;
  }

  .release-list button:hover {
    background: var(--bg-surface-hover);
  }

  .release-list button:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
  }

  .release-list button.active {
    background: color-mix(in srgb, var(--accent-blue) 18%, var(--bg-inset));
  }

  .release-list span {
    font-size: var(--font-size-sm);
    font-weight: 600;
    line-height: 1.35;
  }

  .release-list small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.35;
  }

  .release-detail {
    min-width: 0;
    background: var(--bg-primary);
  }

  .release-detail > header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 20px;
    padding: 18px 20px 14px;
    border-bottom: var(--border-width) solid var(--border-default);
  }

  .release-detail h2 {
    margin: 0;
    font-size: var(--font-size-lg);
    letter-spacing: -0.01em;
  }

  .release-detail p {
    margin: 4px 0 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .release-detail a {
    flex-shrink: 0;
    color: var(--accent-blue);
    font-size: var(--font-size-sm);
    text-underline-offset: 3px;
  }

  .release-copy {
    overflow-y: auto;
    max-height: min(520px, calc(100vh - 230px));
  }

  .release-copy :global(.review-content) {
    max-width: 75ch;
    margin: 0 auto;
    padding: 24px 28px 36px;
  }

  .release-state {
    display: flex;
    min-height: 280px;
    align-items: center;
    justify-content: center;
    flex-direction: column;
    gap: 10px;
    color: var(--text-muted);
    text-align: center;
  }

  .release-state p {
    margin: 0;
  }

  .release-error strong {
    color: var(--text-primary);
  }

  .stale-note {
    margin: 0;
    padding: 8px 12px;
    border-top: var(--border-width) solid var(--accent-amber);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  @media (max-width: 680px) {
    .release-layout {
      min-height: calc(100vh - 112px);
      grid-template-columns: 1fr;
      grid-template-rows: auto minmax(0, 1fr);
    }

    .release-list {
      display: flex;
      overflow-x: auto;
      padding: var(--space-2);
      border-right: 0;
      border-bottom: var(--border-width) solid var(--border-default);
    }

    .release-list button {
      width: 180px;
      flex: 0 0 auto;
    }

    .release-detail > header {
      flex-direction: column;
      gap: 8px;
    }

    .release-copy {
      max-height: calc(100vh - 280px);
    }
  }
</style>
