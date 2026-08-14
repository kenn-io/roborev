<script lang="ts">
  import { Card, type CardTone } from "@kenn-io/kit-ui";

  interface Props {
    label: string;
    value: string;
    detail?: string;
    tone?: CardTone;
  }

  let { label, value, detail = "", tone = "neutral" }: Props = $props();
</script>

<Card
  level="raised"
  eyebrow={label}
  eyebrowTone={tone}
  class="analytics-metric"
>
  <strong class="metric-value">{value}</strong>
  {#if detail}
    <span class="metric-detail">{detail}</span>
  {/if}
</Card>

<style>
  :global(.analytics-metric .kit-card__body) {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .metric-value {
    color: var(--text-primary);
    font-size: clamp(1.35rem, 2.4vw, 2rem);
    font-variant-numeric: tabular-nums;
    line-height: 1;
  }

  .metric-detail {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
