<script lang="ts">
  import {
    Button,
    Card,
    FilterDropdown,
    RefreshControl,
    SegmentedControl,
    SelectDropdown,
    formatCost,
    formatDuration,
    formatNumber,
    type FilterDropdownSection,
    type SegmentedControlOption,
    type SelectDropdownOption,
  } from "@kenn-io/kit-ui";
  import { onMount, untrack } from "svelte";
  import { SvelteSet } from "svelte/reactivity";

  import AnalyticsChart, {
    type ChartPeriod,
    type ChartSeries,
  } from "../components/analytics/AnalyticsChart.svelte";
  import BreakdownTable, {
    type BreakdownRow,
  } from "../components/analytics/BreakdownTable.svelte";
  import MetricCard from "../components/analytics/MetricCard.svelte";
  import type { AnalyticsTimeBucket } from "../api/generated/models";
  import {
    createAnalyticsStore,
    type AnalyticsBucket,
    type AnalyticsFilters,
    type AnalyticsRange,
    type AnalyticsSplit,
    type AnalyticsStore,
  } from "../stores/analytics.svelte";
  import {
    reviewLatency,
    seriesGroups,
    verdictFailureRate,
    type LatencyPercentile,
    type SeriesGroup,
  } from "../utils/analytics-series";

  interface Props {
    store?: AnalyticsStore;
  }

  let { store }: Props = $props();
  const analytics = untrack(() => store ?? createAnalyticsStore({}));
  const filters = $derived(analytics.getFilters());
  const snapshot = $derived(analytics.getSnapshot());
  const loading = $derived(analytics.isLoading());
  const stale = $derived(analytics.isStale());
  const error = $derived(analytics.getError());

  const rangeOptions: SelectDropdownOption[] = [
    { value: "24h", label: "Last 24 hours" },
    { value: "7d", label: "Last 7 days" },
    { value: "30d", label: "Last 30 days" },
    { value: "90d", label: "Last 90 days" },
    { value: "1y", label: "Last year" },
    { value: "all", label: "All time" },
  ];
  const bucketOptions: SelectDropdownOption[] = [
    { value: "auto", label: "Automatic buckets" },
    { value: "hour", label: "Hourly" },
    { value: "day", label: "Daily" },
    { value: "week", label: "Weekly" },
    { value: "month", label: "Monthly" },
  ];
  const splitOptions: SegmentedControlOption[] = [
    { value: "", label: "None" },
    { value: "agent", label: "Agent" },
    { value: "model", label: "Model" },
    { value: "project", label: "Project" },
    { value: "source", label: "Source" },
  ];
  const splitNames: Record<AnalyticsSplit, string> = {
    "": "Project",
    agent: "Agent",
    model: "Model",
    project: "Project",
    source: "Source",
  };
  const latencyOptions: SegmentedControlOption[] = [
    { value: "p50_secs", label: "p50", title: "Median" },
    { value: "p90_secs", label: "p90", title: "90th percentile" },
    { value: "p99_secs", label: "p99", title: "99th percentile" },
  ];
  const durationTickSteps = [
    1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600,
    43200, 86400,
  ];
  const latencyNames: Record<LatencyPercentile, string> = {
    p50_secs: "Median",
    p90_secs: "p90",
    p99_secs: "p99",
  };

  const hiddenSeries = new SvelteSet<string>();
  let latencyPercentile = $state<LatencyPercentile>("p50_secs");
  let emphasizedSeries = $state<string | null>(null);

  const projectSections = $derived(
    filterSections(
      "Projects",
      snapshot?.options.projects ?? [],
      filters.projects,
      "projects",
      (value) => value,
    ),
  );
  const sourceSections = $derived(
    filterSections(
      "Sources",
      snapshot?.options.sources ?? [],
      filters.sources,
      "sources",
      sourceLabel,
    ),
  );
  const agentOptions = $derived(
    selectOptions("All agents", snapshot?.options.agents ?? []),
  );
  const modelOptions = $derived(
    selectOptions("All models", snapshot?.options.models ?? []),
  );

  const activeSplit = $derived<AnalyticsSplit>(
    (snapshot?.filters.split ?? "") as AnalyticsSplit,
  );
  const groups = $derived(
    snapshot ? seriesGroups(snapshot, (value) => splitValueLabel(value)) : [],
  );
  const visibleGroups = $derived(
    groups.filter((group) => !hiddenSeries.has(group.key)),
  );
  const periods = $derived<ChartPeriod[]>(
    (snapshot?.time_series ?? []).map((bucket) => ({
      label: formatBucket(bucket.start, snapshot?.filters.bucket ?? "day"),
      partial:
        Date.parse(bucket.end) > Date.parse(snapshot?.filters.until ?? ""),
    })),
  );
  const volumeSeries = $derived(
    chartSeries(visibleGroups, (bucket) => bucket.reviews.total),
  );
  const costSeries = $derived(
    chartSeries(visibleGroups, (bucket) => bucket.cost.total_usd),
  );
  const failureSeries = $derived(
    chartSeries(visibleGroups, (bucket) => verdictFailureRate(bucket)),
  );
  // Percentiles of a folded "Other" group are unknown, so it has no line.
  const latencySeries = $derived(
    chartSeries(
      visibleGroups.filter((group) => group.members === 1),
      (bucket) => reviewLatency(bucket, latencyPercentile),
    ),
  );
  const breakdownRows = $derived<BreakdownRow[]>(
    snapshot === null
      ? []
      : activeSplit === ""
        ? (snapshot.projects ?? []).map((row) => ({
            key: row.project,
            label: row.project,
            summary: row,
          }))
        : (snapshot.split_series ?? []).map((row) => ({
            key: row.value,
            label: splitValueLabel(row.value),
            color: groups.find((group) => group.key === row.value)?.color,
            summary: row.summary,
          })),
  );

  onMount(() => {
    analytics.start();
    return analytics.dispose;
  });

  function update(update: Partial<AnalyticsFilters>): void {
    void analytics.setFilters(update);
  }

  function setSplit(split: string): void {
    hiddenSeries.clear();
    emphasizedSeries = null;
    update({ split: split as AnalyticsSplit });
  }

  function toggleSeries(key: string): void {
    if (hiddenSeries.has(key)) hiddenSeries.delete(key);
    else hiddenSeries.add(key);
    emphasize(key);
  }

  function emphasize(key: string): void {
    emphasizedSeries = hiddenSeries.has(key) ? null : key;
  }

  function chartSeries(
    source: ReadonlyArray<SeriesGroup>,
    value: (bucket: AnalyticsTimeBucket) => number | null,
  ): ChartSeries[] {
    return source.map((group) => ({
      key: group.key,
      label: group.label,
      color: group.color,
      values: group.buckets.map(value),
    }));
  }

  function toggleFilter(key: "projects" | "sources", value: string): void {
    const current = filters[key];
    update({
      [key]: current.includes(value)
        ? current.filter((item) => item !== value)
        : [...current, value],
    });
  }

  function filterSections(
    title: string,
    options: string[],
    selected: string[],
    key: "projects" | "sources",
    label: (value: string) => string,
  ): FilterDropdownSection[] {
    return [
      {
        title,
        items: options.map((value) => ({
          id: `${key}-${value || "manual"}`,
          label: label(value),
          active: selected.includes(value),
          onSelect: () => toggleFilter(key, value),
        })),
      },
    ];
  }

  function selectOptions(
    allLabel: string,
    values: string[],
  ): SelectDropdownOption[] {
    return [
      { value: "", label: allLabel },
      ...values.filter(Boolean).map((value) => ({ value, label: value })),
    ];
  }

  function splitValueLabel(value: string): string {
    switch (activeSplit) {
      case "source":
        return sourceLabel(value);
      case "model":
        return value || "Default model";
      case "agent":
        return value || "Unknown agent";
      default:
        return value || "Unknown";
    }
  }

  function sourceLabel(source: string): string {
    switch (source) {
      case "":
        return "Manual";
      case "post_commit":
        return "Post-commit";
      case "auto_design":
        return "Auto design";
      case "ci":
        return "CI";
      default:
        return source.replaceAll("_", " ");
    }
  }

  function percentage(value: number): string {
    return `${Math.round(value * 100)}%`;
  }

  function compactNumber(value: number): string {
    return new Intl.NumberFormat(undefined, {
      notation: "compact",
      maximumFractionDigits: 1,
    }).format(value);
  }

  function seconds(value: number): string {
    return formatDuration(value * 1000);
  }

  // Axis ticks land on whole time units (see durationTickSteps).
  function secondsTick(value: number): string {
    if (value === 0) return "0";
    if (value % 3600 === 0) return `${value / 3600}h`;
    if (value % 60 === 0) return `${value / 60}m`;
    return `${value}s`;
  }

  function formatBucket(value: string, bucket: string): string {
    const date = new Date(value);
    if (bucket === "hour") {
      return date.toLocaleString(undefined, {
        month: "short",
        day: "numeric",
        hour: "numeric",
        timeZone: "UTC",
      });
    }
    if (bucket === "week") {
      return `Week of ${date.toLocaleDateString(undefined, {
        month: "short",
        day: "numeric",
        timeZone: "UTC",
      })}`;
    }
    if (bucket === "month") {
      return date.toLocaleDateString(undefined, {
        month: "short",
        year: "numeric",
        timeZone: "UTC",
      });
    }
    return date.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
      timeZone: "UTC",
    });
  }

  function countLabel(value: number, singular: string): string {
    return `${formatNumber(value)} ${singular}${value === 1 ? "" : "s"}`;
  }
</script>

<section class="analytics-view">
  <div class="analytics-page">
    <header class="analytics-header">
      <div>
        <h1>Review analytics</h1>
        <p class="lede">
          Volume, cost, outcomes, and latency from this daemon's review history.
        </p>
      </div>
      <RefreshControl
        lastUpdatedAt={analytics.getLastUpdatedAt()}
        busy={loading}
        onRefresh={() => void analytics.refresh()}
        label="Refresh analytics"
      />
    </header>

    <div class="toolbar">
      <div class="filter-row" role="group" aria-label="Analytics filters">
        <SelectDropdown
          title="Time range"
          value={filters.range}
          options={rangeOptions}
          onchange={(value) => update({ range: value as AnalyticsRange })}
        />
        <SelectDropdown
          title="Time bucket"
          value={filters.bucket}
          options={bucketOptions}
          onchange={(value) => update({ bucket: value as AnalyticsBucket })}
        />
        <span class="toolbar-divider" aria-hidden="true"></span>
        <FilterDropdown
          label="Projects"
          badgeCount={filters.projects.length}
          sections={projectSections}
          searchable
          resetLabel="All projects"
          onReset={() => update({ projects: [] })}
        />
        <FilterDropdown
          label="Sources"
          badgeCount={filters.sources.length}
          sections={sourceSections}
          resetLabel="All sources"
          onReset={() => update({ sources: [] })}
        />
        <SelectDropdown
          title="Agent"
          value={filters.agent}
          options={agentOptions}
          onchange={(agent) => update({ agent })}
        />
        <SelectDropdown
          title="Model"
          value={filters.model}
          options={modelOptions}
          onchange={(model) => update({ model })}
        />
      </div>
      <div class="split-control">
        <span class="split-label" id="split-label">Break down by</span>
        <SegmentedControl
          ariaLabel="Break down by"
          options={splitOptions}
          value={filters.split}
          onchange={setSplit}
        />
      </div>
    </div>

    {#if stale && snapshot}
      <div class="stale-notice" role="status">
        <span
          >Showing stale data because the latest refresh failed{error
            ? `: ${error}`
            : "."}</span
        >
        <Button size="sm" onclick={() => void analytics.refresh()}>Retry</Button
        >
      </div>
    {/if}

    {#if snapshot === null && loading}
      <div class="state-panel" aria-live="polite">Loading analytics…</div>
    {:else if snapshot === null && error}
      <div class="state-panel" role="alert">
        <h2>Analytics unavailable</h2>
        <p>{error}</p>
        <Button tone="info" onclick={() => void analytics.refresh()}
          >Retry</Button
        >
      </div>
    {:else if snapshot && snapshot.summary.reviews.total === 0 && snapshot.summary.attempts.eligible === 0}
      <div class="state-panel">
        <h2>No reviews in this range</h2>
        <p>Change the time range or remove filters to see review activity.</p>
      </div>
    {:else if snapshot}
      {@const verdicts = snapshot.summary.verdicts}
      {@const failedVerdicts = verdicts.fail_open + verdicts.fail_closed}
      <div class="metric-grid">
        <MetricCard
          label="Reviews"
          value={formatNumber(snapshot.summary.reviews.total)}
          detail={`${countLabel(snapshot.summary.reviews.run_errors, "run error")} · ${snapshot.summary.reviews.canceled} canceled · ${snapshot.summary.reviews.skipped} skipped`}
        />
        <MetricCard
          label="Failure rate"
          value={percentage(verdicts.failure_rate)}
          detail={`${failedVerdicts} failed verdicts of ${verdicts.rated} rated reviews`}
        >
          {#if verdicts.rated > 0}
            <div
              class="outcome-meter"
              role="img"
              aria-label={`${verdicts.passed} passed, ${verdicts.fail_closed} failed and addressed, ${verdicts.fail_open} failed and open`}
            >
              <span
                class="outcome pass"
                style:flex-grow={verdicts.passed}
                title={`${verdicts.passed} passed`}
              ></span>
              <span
                class="outcome addressed"
                style:flex-grow={verdicts.fail_closed}
                title={`${verdicts.fail_closed} failed, addressed`}
              ></span>
              <span
                class="outcome open"
                style:flex-grow={verdicts.fail_open}
                title={`${verdicts.fail_open} failed, open`}
              ></span>
            </div>
            <dl class="outcome-legend">
              <div>
                <dt class="pass">Pass</dt>
                <dd>{verdicts.passed}</dd>
              </div>
              <div>
                <dt class="addressed">Fail, addressed</dt>
                <dd>{verdicts.fail_closed}</dd>
              </div>
              <div>
                <dt class="open">Fail, open</dt>
                <dd>{verdicts.fail_open}</dd>
              </div>
            </dl>
          {/if}
        </MetricCard>
        <MetricCard
          label="Median review latency"
          value={seconds(snapshot.summary.review_latency.p50_secs)}
          detail={`p90 ${seconds(snapshot.summary.review_latency.p90_secs)} · p99 ${seconds(snapshot.summary.review_latency.p99_secs)}`}
        />
        <MetricCard
          label="Estimated cost"
          value={formatCost(snapshot.summary.cost.total_usd)}
          detail={`${snapshot.summary.cost.priced_attempts} of ${snapshot.summary.cost.eligible_attempts} eligible attempts priced`}
        >
          <span
            class="coverage"
            class:incomplete={!snapshot.summary.cost.complete}
          >
            {snapshot.summary.cost.complete
              ? "All eligible attempts priced"
              : "Estimated cost is a lower bound"}
          </span>
        </MetricCard>
      </div>

      {#if groups.length > 1}
        <div class="legend" role="group" aria-label="Chart series">
          {#each groups as group (group.key)}
            <button
              type="button"
              class="legend-item"
              class:off={hiddenSeries.has(group.key)}
              aria-pressed={!hiddenSeries.has(group.key)}
              title={hiddenSeries.has(group.key)
                ? `Show ${group.label}`
                : `Hide ${group.label}`}
              onclick={() => toggleSeries(group.key)}
              onpointerenter={() => emphasize(group.key)}
              onpointerleave={() => (emphasizedSeries = null)}
              onfocus={() => emphasize(group.key)}
              onblur={() => (emphasizedSeries = null)}
            >
              <span
                class="swatch"
                style:background={hiddenSeries.has(group.key)
                  ? undefined
                  : group.color}
              ></span>
              <span class="legend-label">{group.label}</span>
              <span class="legend-count"
                >{formatNumber(group.summary.reviews.total)}</span
              >
            </button>
          {/each}
          {#if hiddenSeries.size > 0}
            <button
              type="button"
              class="legend-reset"
              onclick={() => hiddenSeries.clear()}>Show all</button
            >
          {/if}
        </div>
      {/if}

      <div class="chart-grid">
        <Card level="raised" title="Review volume" meta="Logical reviews">
          <AnalyticsChart
            label="Logical reviews over time"
            kind="bars"
            {periods}
            emphasis={emphasizedSeries}
            series={volumeSeries}
            formatValue={formatNumber}
            formatTick={compactNumber}
          />
        </Card>
        <Card
          level="raised"
          title="Estimated cost"
          meta={`${snapshot.summary.cost.priced_attempts} priced attempts`}
        >
          <AnalyticsChart
            label="Estimated cost over time"
            kind="bars"
            {periods}
            emphasis={emphasizedSeries}
            series={costSeries}
            formatValue={formatCost}
            formatTick={formatCost}
          />
        </Card>
        <Card
          level="raised"
          title="Review failure rate"
          meta="Failed verdicts of rated reviews"
        >
          <AnalyticsChart
            label="Review failure rate over time"
            kind="lines"
            {periods}
            emphasis={emphasizedSeries}
            series={failureSeries}
            formatValue={percentage}
            formatTick={percentage}
            maxValue={1}
          />
        </Card>
        <Card
          level="raised"
          title={`${latencyNames[latencyPercentile]} review latency`}
        >
          {#snippet actions()}
            <SegmentedControl
              ariaLabel="Latency percentile"
              options={latencyOptions}
              value={latencyPercentile}
              onchange={(value) =>
                (latencyPercentile = value as LatencyPercentile)}
            />
          {/snippet}
          <AnalyticsChart
            label={`${latencyNames[latencyPercentile]} review latency over time`}
            kind="lines"
            {periods}
            emphasis={emphasizedSeries}
            series={latencySeries}
            formatValue={seconds}
            formatTick={secondsTick}
            tickSteps={durationTickSteps}
          />
        </Card>
      </div>

      <Card
        level="raised"
        padding="none"
        class="breakdown-card"
        title={activeSplit === ""
          ? "Projects"
          : `By ${splitNames[activeSplit].toLowerCase()}`}
        meta={activeSplit === ""
          ? "Choose Break down by to compare agents, models, or sources"
          : `${breakdownRows.length} ${breakdownRows.length === 1 ? "value" : "values"}`}
      >
        <BreakdownTable
          ariaLabel={activeSplit === ""
            ? "Project analytics"
            : `${splitNames[activeSplit]} analytics`}
          dimensionLabel={splitNames[activeSplit]}
          rows={breakdownRows}
        />
      </Card>
    {/if}
  </div>
</section>

<style>
  .analytics-view {
    width: 100%;
    min-width: 0;
    overflow: auto;
  }

  .analytics-page {
    display: grid;
    max-width: 88rem;
    margin: 0 auto;
    padding: var(--space-7) clamp(1rem, 3vw, 2rem) var(--space-8);
    gap: var(--space-6);
  }

  .analytics-header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--space-6);
  }

  h1,
  h2,
  p {
    margin-top: 0;
  }

  h1 {
    margin-bottom: var(--space-2);
    font-size: var(--font-size-2xl);
    font-weight: 600;
    letter-spacing: -0.01em;
  }

  .lede,
  .state-panel p {
    margin-bottom: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-md);
  }

  .toolbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-5) var(--space-7);
    flex-wrap: wrap;
  }

  .filter-row {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    flex-wrap: wrap;
  }

  .filter-row :global(.kit-select-dropdown) {
    min-width: 9rem;
  }

  .toolbar-divider {
    width: 1px;
    height: 20px;
    background: var(--border-default);
  }

  .split-control {
    display: flex;
    align-items: center;
    gap: var(--space-4);
  }

  .split-label {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    white-space: nowrap;
  }

  .stale-notice {
    display: flex;
    padding: var(--space-4) var(--space-6);
    align-items: center;
    justify-content: space-between;
    gap: var(--space-6);
    border: 1px solid
      color-mix(in srgb, var(--accent-amber) 45%, var(--border-default));
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-amber) 8%, var(--bg-surface));
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .state-panel {
    display: grid;
    min-height: 18rem;
    margin-top: var(--space-8);
    padding: var(--space-8);
    place-items: center;
    align-content: center;
    gap: var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    background: var(--bg-surface);
    text-align: center;
  }

  .state-panel h2 {
    margin-bottom: 0;
    font-size: var(--font-size-lg);
    font-weight: 600;
  }

  .metric-grid {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: var(--space-6);
  }

  .outcome-meter {
    display: flex;
    height: 6px;
    overflow: hidden;
    gap: 2px;
    border-radius: 3px;
  }

  .outcome {
    min-width: 2px;
  }

  .outcome.pass,
  .outcome-legend .pass::before {
    background: var(--verdict-pass);
  }

  .outcome.addressed,
  .outcome-legend .addressed::before {
    background: var(--accent-amber);
  }

  .outcome.open,
  .outcome-legend .open::before {
    background: var(--verdict-fail);
  }

  .outcome-legend {
    display: flex;
    margin: 0;
    gap: var(--space-2) var(--space-6);
    flex-wrap: wrap;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .outcome-legend div {
    display: flex;
    gap: var(--space-3);
  }

  .outcome-legend dt::before {
    display: inline-block;
    width: 6px;
    height: 6px;
    margin-right: var(--space-3);
    border-radius: var(--radius-dot, 50%);
    content: "";
    vertical-align: 0.1em;
  }

  .outcome-legend dd {
    margin: 0;
    color: var(--text-secondary);
    font-variant-numeric: tabular-nums;
  }

  .coverage {
    color: var(--accent-green);
    font-size: var(--font-size-xs);
    font-weight: 500;
  }

  .coverage.incomplete {
    color: var(--accent-amber);
  }

  .legend {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .legend-item {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
    padding: 3px 10px;
    border: 1px solid var(--border-default);
    border-radius: 999px;
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    cursor: pointer;
    transition:
      background var(--transition-fast),
      opacity var(--transition-fast);
  }

  .legend-item:hover {
    background: var(--bg-surface-hover);
  }

  .legend-item:focus-visible,
  .legend-reset:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 1px;
  }

  .legend-item.off {
    opacity: 0.5;
  }

  .legend-item.off .swatch {
    box-shadow: inset 0 0 0 1.5px var(--text-muted);
  }

  .legend-count {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }

  .legend-reset {
    padding: 3px 8px;
    border: 0;
    background: transparent;
    color: var(--accent-blue);
    font: inherit;
    font-size: var(--font-size-sm);
    cursor: pointer;
  }

  .swatch {
    width: 8px;
    height: 8px;
    flex-shrink: 0;
    border-radius: 2px;
  }

  .chart-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-6);
  }

  .chart-grid :global(.kit-card),
  .analytics-page > :global(.breakdown-card) {
    gap: var(--space-5);
  }

  .analytics-page > :global(.breakdown-card .kit-card__header) {
    padding: var(--space-5) var(--space-6) 0;
  }

  @media (max-width: 70rem) {
    .metric-grid {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }

  @media (max-width: 48rem) {
    .analytics-header {
      flex-direction: column;
    }

    .metric-grid,
    .chart-grid {
      grid-template-columns: 1fr;
    }

    .filter-row :global(.kit-select-dropdown) {
      flex: 1 1 10rem;
    }

    .toolbar-divider {
      display: none;
    }
  }
</style>
