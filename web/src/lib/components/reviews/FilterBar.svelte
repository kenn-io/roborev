<script lang="ts">
  import { Effect } from "effect";
  import { onDestroy } from "svelte";
  import { getAppRuntime } from "../../runtime/context";
  import type { AppExecution } from "../../runtime/runtime";
  import { getReviewStores } from "../../stores/context";
  import { Checkbox, FilterDropdown, SearchInput } from "@kenn-io/kit-ui";
  import RepoTreePicker from "./RepoTreePicker.svelte";

  interface Props {
    onHelpClick?: () => void;
    disabled?: boolean;
  }
  let { onHelpClick, disabled = false }: Props = $props();

  const stores = getReviewStores();
  const jobsStore = stores.roborevJobs;
  const runtime = getAppRuntime();

  const statusOptions = [
    { value: "", label: "All statuses" },
    { value: "queued", label: "Queued" },
    { value: "running", label: "Running" },
    { value: "done", label: "Done" },
    { value: "failed", label: "Failed" },
    { value: "canceled", label: "Canceled" },
  ];

  let searchValue = $state(jobsStore?.getFilterSearch() ?? "");
  let searchExecution: AppExecution<void, never> | undefined;

  function setStatusFilter(value: string): void {
    jobsStore?.setFilter("status", value || undefined);
  }

  const statusDetail = $derived.by(() => {
    const current = jobsStore?.getFilterStatus() ?? "";
    if (current === "") return undefined;
    return statusOptions.find((opt) => opt.value === current)?.label;
  });

  const statusSections = $derived.by(() => [
    {
      items: statusOptions.map((opt) => ({
        id: opt.value || "all-statuses",
        label: opt.label,
        active: (jobsStore?.getFilterStatus() ?? "") === opt.value,
        color:
          opt.value === "queued"
            ? "var(--accent-amber)"
            : opt.value === "running"
              ? "var(--accent-blue)"
              : opt.value === "done"
                ? "var(--accent-green)"
                : opt.value === "failed"
                  ? "var(--accent-red)"
                  : opt.value === "canceled"
                    ? "var(--text-muted)"
                    : "var(--accent-blue)",
        closeOnSelect: true,
        onSelect: () => setStatusFilter(opt.value),
      })),
    },
  ]);

  function onSearchInput(value: string): void {
    searchValue = value;
    searchExecution?.interrupt();
    searchExecution = runtime.runCommand(
      Effect.sleep("300 millis").pipe(
        Effect.andThen(
          Effect.sync(() => {
            jobsStore?.setFilter("search", value || undefined);
          }),
        ),
      ),
      {
        operation: "apply Roborev search filter",
        safeContext: {},
        onFailure: () => {},
      },
    );
  }

  onDestroy(() => searchExecution?.interrupt());

  function onHideClosedChange(checked: boolean): void {
    jobsStore?.setFilter("hideClosed", checked);
  }

  function onShowAutoDesignChange(checked: boolean): void {
    jobsStore?.setFilter("showAutoDesign", checked);
  }
</script>

<div class="filter-bar">
  <div class:filter-disabled={disabled}>
    <RepoTreePicker />
  </div>

  <FilterDropdown
    label="Status"
    active={(jobsStore?.getFilterStatus() ?? "") !== ""}
    showBadge={false}
    sections={statusSections}
    title="Filter reviews by status"
    minWidth="170px"
    {disabled}
    {...statusDetail ? { detail: statusDetail } : {}}
  />

  <div class="search-wrap">
    <SearchInput
      bind:value={searchValue}
      size="sm"
      block
      placeholder="Search by ref..."
      ariaLabel="Search by ref"
      oninput={onSearchInput}
      {disabled}
    />
  </div>

  <Checkbox
    class="filter-checkbox"
    checked={jobsStore?.getFilterHideClosed() ?? false}
    label="Hide closed"
    onchange={onHideClosedChange}
    {disabled}
  />

  <Checkbox
    class="filter-checkbox"
    checked={jobsStore?.getFilterShowAutoDesign() ?? false}
    label="Show auto-design"
    onchange={onShowAutoDesignChange}
    {disabled}
  />

  <button class="help-btn" title="Keyboard shortcuts" onclick={onHelpClick}>
    ?
  </button>
</div>

<style>
  .filter-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-surface);
    flex-shrink: 0;
    flex-wrap: wrap;
  }

  .search-wrap {
    min-width: 140px;
    flex: 1;
    max-width: 220px;
  }

  :global(.filter-checkbox) {
    gap: var(--space-2);
    white-space: nowrap;
    user-select: none;
  }

  :global(.filter-checkbox .kit-checkbox__label) {
    color: var(--text-secondary);
  }

  .help-btn {
    width: 24px;
    height: 24px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    font-weight: 600;
    cursor: pointer;
    flex-shrink: 0;
    margin-left: auto;
  }

  .help-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .filter-disabled {
    pointer-events: none;
    opacity: 0.5;
  }
</style>
