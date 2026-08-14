<script lang="ts">
  import { untrack } from "svelte";
  import type { AppRuntime } from "../../runtime/runtime";
  import { setAppRuntime } from "../../runtime/context";
  import FilterBar from "./FilterBar.svelte";

  const { runtime }: { runtime: AppRuntime } = $props();

  setAppRuntime(untrack(() => runtime));
</script>

<FilterBar />
