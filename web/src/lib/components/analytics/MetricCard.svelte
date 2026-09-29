<script lang="ts">
  import type { Snippet } from "svelte";

  interface Props {
    label: string;
    value: string;
    detail?: string;
    children?: Snippet;
  }

  let { label, value, detail = "", children }: Props = $props();
</script>

<section class="metric" aria-label={label}>
  <h2 class="metric-label">{label}</h2>
  <strong class="metric-value">{value}</strong>
  {#if children}{@render children()}{/if}
  {#if detail}
    <span class="metric-detail">{detail}</span>
  {/if}
</section>

<style>
  .metric {
    display: flex;
    min-width: 0;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-6) var(--space-7);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    background: var(--bg-surface);
    box-shadow: var(--shadow-sm);
  }

  .metric-label {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: 500;
  }

  .metric-value {
    color: var(--text-primary);
    font-size: var(--font-size-2xl);
    font-weight: 600;
    font-variant-numeric: tabular-nums;
    letter-spacing: -0.01em;
    line-height: 1.15;
  }

  .metric-detail {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }
</style>
