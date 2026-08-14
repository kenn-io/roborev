<script lang="ts">
  import StatusBadge from "@kenn-io/roborev-ui/status-badge";

  interface Props {
    status: string;
  }

  let { status }: Props = $props();
</script>

<StatusBadge {status} />
