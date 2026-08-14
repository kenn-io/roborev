<script lang="ts">
  import { formatRelativeTime } from "@kenn-io/kit-ui";
  import type { ReviewProjectionResponse } from "../types";

  interface Props {
    responses: ReadonlyArray<ReviewProjectionResponse>;
  }

  let { responses }: Props = $props();
</script>

<div class="response-list">
  {#if responses.length > 0}
    <div class="responses">
      {#each responses as response (response.id)}
        <article class="response-item">
          <header class="response-header">
            <span class="responder">{response.responder}</span>
            <time class="timestamp" datetime={response.created_at}>
              {formatRelativeTime(response.created_at)}
            </time>
          </header>
          <div class="response-body">{response.response}</div>
        </article>
      {/each}
    </div>
  {:else}
    <div class="no-responses">No comments yet.</div>
  {/if}
</div>

<style>
  .response-list,
  .responses {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .response-item {
    padding: 8px 12px;
    border: 1px solid var(--border-muted, #30363d);
    border-radius: var(--radius-sm, 4px);
    background: var(--bg-surface, #161b22);
  }

  .response-header {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 4px;
  }

  .responder {
    color: var(--text-primary, #f0f6fc);
    font-size: var(--font-size-sm, 0.8125rem);
    font-weight: 600;
  }

  .timestamp,
  .no-responses {
    color: var(--text-muted, #8b949e);
    font-size: var(--font-size-xs, 0.75rem);
  }

  .response-body {
    color: var(--text-secondary, #c9d1d9);
    font-size: var(--font-size-md, 0.875rem);
    line-height: 1.5;
    white-space: pre-wrap;
  }

  .no-responses {
    padding: 12px 0;
    text-align: center;
  }
</style>
