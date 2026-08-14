<script lang="ts">
  import VerdictBadge from "@kenn-io/roborev-ui/verdict-badge";

  interface Props {
    verdict: string | undefined;
  }

  let { verdict }: Props = $props();
</script>

<VerdictBadge {verdict} />
