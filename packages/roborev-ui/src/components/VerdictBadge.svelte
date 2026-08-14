<script lang="ts">
  interface Props {
    verdict: string | undefined;
  }
  let { verdict }: Props = $props();

  const isPass = $derived(verdict === "pass" || verdict === "P");
  const isFail = $derived(verdict === "fail" || verdict === "F");
</script>

{#if isPass}
  <span class="verdict verdict-pass" title="Pass">
    <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
      <path
        d="M13.78 4.22a.75.75 0 010 1.06l-7.25
        7.25a.75.75 0 01-1.06 0L2.22
        9.28a.75.75 0 011.06-1.06L6 10.94l6.72-6.72a.75.75
        0 011.06 0z"
      />
    </svg>
    Pass
  </span>
{:else if isFail}
  <span class="verdict verdict-fail" title="Fail">
    <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
      <path
        d="M3.72 3.72a.75.75 0 011.06 0L8
        6.94l3.22-3.22a.75.75 0 111.06 1.06L9.06
        8l3.22 3.22a.75.75 0 11-1.06 1.06L8
        9.06l-3.22 3.22a.75.75 0 01-1.06-1.06L6.94
        8 3.72 4.78a.75.75 0 010-1.06z"
      />
    </svg>
    Fail
  </span>
{:else}
  <span class="verdict verdict-none" title="No verdict"> -- </span>
{/if}

<style>
  .verdict {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--font-size-xs);
    font-weight: 500;
    white-space: nowrap;
  }

  .verdict-pass {
    color: var(--verdict-pass);
  }

  .verdict-fail {
    color: var(--verdict-fail);
  }

  .verdict-none {
    color: var(--text-muted);
  }
</style>
