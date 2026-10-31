<script lang="ts">
  import type { Commit } from "../../api/generated/models";

  let { commits = [] }: { commits?: Commit[] } = $props();

  function sessionURL(raw?: string): string | undefined {
    if (!raw) return undefined;
    try {
      const url = new URL(raw);
      return (url.protocol === "http:" || url.protocol === "https:") &&
        !url.username &&
        !url.password
        ? url.href
        : undefined;
    } catch {
      return undefined;
    }
  }
</script>

{#if commits.length > 0}
  <section class="session-provenance" aria-label="Agent session provenance">
    <h3>Agent sessions behind these commits</h3>
    {#each commits as commit (commit.commit_sha)}
      <div class="commit-provenance">
        <code>{commit.commit_sha}</code>
        {#if commit.coverage === "why-selected"}
          <p class="coverage">
            agentsview why selection: up to three sessions, one source card per
            session.
          </p>
        {:else if commit.coverage === "http"}
          <p class="coverage">agentsview commit lookup</p>
        {:else}
          <p class="coverage">
            Trailers only; archive enrichment unavailable or not queried.
          </p>
        {/if}
        {#each commit.sessions as session (JSON.stringify(session))}
          {@const href = sessionURL(session.web_url)}
          <div class="session-evidence">
            <p>
              {#if href}
                <a {href} target="_blank" rel="noopener noreferrer"
                  >{session.session_id}</a
                >
              {:else}
                <code>{session.session_id}</code>
              {/if}
              <span> ({session.agent}; {session.source})</span>
            </p>
            {#if session.ambiguous}<p>Ambiguous commit match</p>{/if}
            {#if session.availability === "gone"}
              <p>Session unavailable</p>
            {:else if session.availability === "unknown"}
              <p>Session availability unknown</p>
            {/if}
            {#if !session.resolved || session.commit_sha !== commit.commit_sha}
              <p>Evidence commit: <code>{session.commit_sha}</code></p>
            {/if}
            {#if !href && session.web_url}<p>{session.web_url}</p>{/if}
            {#if session.turn}
              <p class="turn-label">
                Writing prompt {session.turn.ordinal} ({session.turn.why})
              </p>
              <p class="writing-prompt">{session.turn.prompt}</p>
            {/if}
          </div>
        {/each}
      </div>
    {/each}
  </section>
{/if}

<style>
  .session-provenance {
    padding: var(--space-4);
    border-bottom: 1px solid var(--border-color);
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
  }
  h3 {
    margin: 0 0 var(--space-3);
    font-size: var(--font-size-md);
  }
  p {
    margin: var(--space-1) 0;
  }
  .commit-provenance + .commit-provenance {
    margin-top: var(--space-4);
  }
  .coverage,
  .turn-label {
    color: var(--text-muted);
  }
  .session-evidence {
    margin-top: var(--space-3);
  }
  .writing-prompt {
    white-space: pre-wrap;
  }
</style>
