<script lang="ts">
  import type { ReviewProjectionJob } from "../types";
  import VerdictBadge from "./VerdictBadge.svelte";

  interface Props {
    panelName?: string;
    members: ReadonlyArray<ReviewProjectionJob>;
  }

  let { panelName, members }: Props = $props();
</script>

{#if members.length > 0}
  <section class="panel-attribution" aria-label="Panel reviewers">
    <strong>{panelName ? `${panelName} panel` : "Review panel"}</strong>
    <ul>
      {#each members as member (member.id)}
        <li>
          <span>{member.panel_member_name || member.agent}</span>
          <VerdictBadge verdict={member.verdict} />
        </li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .panel-attribution {
    display: grid;
    gap: 8px;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border-default, #30363d);
    color: var(--text-primary, #f0f6fc);
  }

  ul {
    display: flex;
    flex-wrap: wrap;
    gap: 8px 16px;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    color: var(--text-secondary, #c9d1d9);
    font-size: var(--font-size-sm, 0.8125rem);
  }
</style>
