<script lang="ts">
  import { Chip, type ChipTone } from "@kenn-io/kit-ui";

  interface Props {
    status: string;
  }
  let { status }: Props = $props();

  const tone: ChipTone = $derived.by(() => {
    switch (status) {
      case "queued":
        return "warning";
      case "running":
        return "info";
      case "done":
        return "success";
      case "failed":
        return "danger";
      case "canceled":
        return "canceled";
      default:
        return "muted";
    }
  });
</script>

<Chip
  size="xs"
  {tone}
  dot
  uppercase={false}
  class={`status-badge status-${status} review-status-chip review-status-chip--${status}`}
>
  {status}
</Chip>
