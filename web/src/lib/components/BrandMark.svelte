<script lang="ts">
  import ScanEyeIcon from "@lucide/svelte/icons/scan-eye";
</script>

<span class="brand-mark" aria-hidden="true">
  <ScanEyeIcon size="14" strokeWidth="2.25" />
</span>

<style>
  .brand-mark {
    display: inline-grid;
    width: 22px;
    height: 22px;
    flex-shrink: 0;
    place-items: center;
    border-radius: var(--radius-md);
    background: var(--accent-blue);
    color: var(--bg-surface);
  }
</style>
