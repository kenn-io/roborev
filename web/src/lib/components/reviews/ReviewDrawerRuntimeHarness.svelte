<script lang="ts">
  import { untrack } from "svelte";
  import type { AppRuntime } from "../../runtime/runtime";
  import { setAppRuntime } from "../../runtime/context";
  import ReviewDrawer from "./ReviewDrawer.svelte";

  const { runtime }: { runtime: AppRuntime } = $props();

  setAppRuntime(untrack(() => runtime));
</script>

<ReviewDrawer />
