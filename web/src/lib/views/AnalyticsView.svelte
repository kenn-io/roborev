<script lang="ts">
  import {
    Button,
    Card,
    FilterDropdown,
    RefreshControl,
    SelectDropdown,
    Table,
    TableHeaderCell,
    formatCost,
    formatDuration,
    formatNumber,
    type FilterDropdownSection,
    type SelectDropdownOption,
  } from "@kenn-io/kit-ui";
  import { onMount, untrack } from "svelte";

  import MetricCard from "../components/analytics/MetricCard.svelte";
  import TimeSeriesChart from "../components/analytics/TimeSeriesChart.svelte";
  import { getRoborevClient } from "../runtime/context";
  import {
    createAnalyticsStore,
    type AnalyticsBucket,
    type AnalyticsFilters,
    type AnalyticsRange,
    type AnalyticsStore,
  } from "../stores/analytics.svelte";

  interface Props {
    store?: AnalyticsStore;
  }

  let { store }: Props = $props();
  const analytics = untrack(
    () => store ?? createAnalyticsStore({ client: getRoborevClient() }),
  );
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
  const volumePoints = $derived(
    (snapshot?.time_series ?? []).map((bucket) => ({
      label: formatBucket(bucket.start, snapshot?.filters.bucket ?? "day"),
      value: bucket.reviews.total,
    })),
  );
  const costPoints = $derived(
    (snapshot?.time_series ?? []).map((bucket) => ({
      label: formatBucket(bucket.start, snapshot?.filters.bucket ?? "day"),
      value: bucket.cost.total_usd,
    })),
  );
  const failurePoints = $derived(
    (snapshot?.time_series ?? []).map((bucket) => ({
      label: formatBucket(bucket.start, snapshot?.filters.bucket ?? "day"),
      value: bucket.verdicts.failure_rate,
    })),
  );
  const latencyPoints = $derived(
    (snapshot?.time_series ?? []).map((bucket) => ({
      label: formatBucket(bucket.start, snapshot?.filters.bucket ?? "day"),
      value: bucket.review_latency.p50_secs,
    })),
  );

  onMount(() => {
    analytics.start();
    return analytics.dispose;
  });

  function update(update: Partial<AnalyticsFilters>): void {
    void analytics.setFilters(update);
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
    return `${value} ${singular}${value === 1 ? "" : "s"}`;
  }
</script>

<section class="analytics-view">
  <header class="analytics-header">
    <div>
      <p class="eyebrow">Analytics</p>
      <h1>Project review health</h1>
      <p class="lede">
        Cost, latency, reliability, and outcomes from this daemon's review
        history.
      </p>
    </div>
    <RefreshControl
      lastUpdatedAt={analytics.getLastUpdatedAt()}
      busy={loading}
      onRefresh={() => void analytics.refresh()}
      label="Refresh analytics"
    />
  </header>

  <div class="filter-row" aria-label="Analytics filters">
    <SelectDropdown
      title="Time range"
      value={filters.range}
      options={rangeOptions}
      onchange={(value) => update({ range: value as AnalyticsRange })}
    />
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
    <SelectDropdown
      title="Time bucket"
      value={filters.bucket}
      options={bucketOptions}
      onchange={(value) => update({ bucket: value as AnalyticsBucket })}
    />
  </div>

  {#if stale && snapshot}
    <div class="stale-notice" role="status">
      <span
        >Showing stale data because the latest refresh failed{error
          ? `: ${error}`
          : "."}</span
      >
      <Button size="sm" onclick={() => void analytics.refresh()}>Retry</Button>
    </div>
  {/if}

  {#if snapshot === null && loading}
    <div class="state-panel" aria-live="polite">Loading analytics…</div>
  {:else if snapshot === null && error}
    <div class="state-panel" role="alert">
      <h2>Analytics unavailable</h2>
      <p>{error}</p>
      <Button tone="info" onclick={() => void analytics.refresh()}>Retry</Button
      >
    </div>
  {:else if snapshot && snapshot.summary.reviews.total === 0 && snapshot.summary.attempts.eligible === 0}
    <div class="state-panel">
      <h2>No reviews in this range</h2>
      <p>Change the time range or remove filters to see review activity.</p>
    </div>
  {:else if snapshot}
    <div class="metric-grid">
      <MetricCard
        label="Logical reviews"
        value={formatNumber(snapshot.summary.reviews.total)}
        detail={`${countLabel(snapshot.summary.reviews.run_errors, "run error")} · ${snapshot.summary.reviews.canceled} canceled · ${snapshot.summary.reviews.skipped} skipped`}
      />
      <MetricCard
        label="Failure rate"
        value={percentage(snapshot.summary.verdicts.failure_rate)}
        detail={`${snapshot.summary.verdicts.fail_open + snapshot.summary.verdicts.fail_closed} failed verdicts of ${snapshot.summary.verdicts.rated} rated reviews`}
      />
      <MetricCard
        label="Median review latency"
        value={formatDuration(snapshot.summary.review_latency.p50_secs * 1000)}
        detail={`p90 ${formatDuration(snapshot.summary.review_latency.p90_secs * 1000)} · p99 ${formatDuration(snapshot.summary.review_latency.p99_secs * 1000)}`}
        tone="info"
      />
      <MetricCard
        label="Estimated cost"
        value={formatCost(snapshot.summary.cost.total_usd)}
        detail={`${snapshot.summary.cost.priced_attempts} of ${snapshot.summary.cost.eligible_attempts} eligible attempts priced`}
        tone="warning"
      />
      <MetricCard
        label="Pricing coverage"
        value={`${percentage(snapshot.summary.cost.coverage)} coverage`}
        detail={snapshot.summary.cost.complete
          ? "All eligible attempts priced"
          : "Estimated cost is a lower bound"}
        tone={snapshot.summary.cost.complete ? "success" : "warning"}
      />
    </div>

    <div class="chart-grid">
      <Card
        level="raised"
        title="Review volume"
        meta={`${snapshot.time_series?.length ?? 0} buckets`}
      >
        <TimeSeriesChart
          label="Logical reviews over time"
          points={volumePoints}
          formatValue={formatNumber}
          formatTick={compactNumber}
        />
      </Card>
      <Card
        level="raised"
        title="Estimated cost"
        meta={`${snapshot.summary.cost.priced_attempts} priced attempts`}
      >
        <TimeSeriesChart
          label="Estimated cost over time"
          points={costPoints}
          formatValue={formatCost}
          formatTick={formatCost}
          tone="amber"
        />
      </Card>
      <Card
        level="raised"
        title="Review failure rate"
        meta={`${snapshot.summary.verdicts.fail_open + snapshot.summary.verdicts.fail_closed} failed verdicts of ${snapshot.summary.verdicts.rated} rated reviews`}
      >
        <TimeSeriesChart
          label="Review failure rate over time"
          points={failurePoints}
          formatValue={percentage}
          formatTick={percentage}
          minValue={0}
          maxValue={1}
        />
      </Card>
      <Card
        level="raised"
        title="Median review latency"
        meta="Enqueued to finished"
      >
        <TimeSeriesChart
          label="Median review latency over time"
          points={latencyPoints}
          formatValue={(seconds) => formatDuration(seconds * 1000)}
          formatTick={(seconds) => formatDuration(seconds * 1000)}
          tone="green"
        />
      </Card>
    </div>

    <Card
      level="raised"
      padding="none"
      title="Projects"
      meta="Grouped by displayed project name"
    >
      <Table ariaLabel="Project analytics" stickyHeader={false}>
        {#snippet header()}
          <TableHeaderCell label="Project" />
          <TableHeaderCell label="Reviews" numeric />
          <TableHeaderCell label="Failure rate" numeric />
          <TableHeaderCell label="Median latency" numeric />
          <TableHeaderCell label="Estimated cost" numeric />
          <TableHeaderCell label="Pricing coverage" numeric />
        {/snippet}
        {#each snapshot.projects ?? [] as project (project.project)}
          <tr>
            <td>{project.project}</td>
            <td class="numeric">{formatNumber(project.reviews.total)}</td>
            <td class="numeric">{percentage(project.verdicts.failure_rate)}</td>
            <td class="numeric"
              >{formatDuration(project.review_latency.p50_secs * 1000)}</td
            >
            <td class="numeric">{formatCost(project.cost.total_usd)}</td>
            <td class="numeric">{percentage(project.cost.coverage)}</td>
          </tr>
        {/each}
      </Table>
    </Card>

    <div class="breakdown-grid">
      <Card level="raised" title="Review outcomes">
        <dl class="breakdown-list">
          <div>
            <dt>Pass</dt>
            <dd>{snapshot.summary.verdicts.passed}</dd>
          </div>
          <div>
            <dt>Fail, open</dt>
            <dd>{snapshot.summary.verdicts.fail_open}</dd>
          </div>
          <div>
            <dt>Fail, addressed</dt>
            <dd>{snapshot.summary.verdicts.fail_closed}</dd>
          </div>
        </dl>
      </Card>
      <Card level="raised" title="Sources">
        <dl class="breakdown-list">
          {#each snapshot.sources ?? [] as source (source.value)}
            <div>
              <dt>{sourceLabel(source.value)}</dt>
              <dd>{source.reviews.total}</dd>
            </div>
          {/each}
        </dl>
      </Card>
      <Card level="raised" title="Agents and models">
        <dl class="breakdown-list">
          {#each snapshot.agents ?? [] as agent (`agent-${agent.value}`)}
            <div>
              <dt>{agent.value || "Unknown agent"}</dt>
              <dd>{formatCost(agent.cost.total_usd)}</dd>
            </div>
          {/each}
          {#each snapshot.models ?? [] as model (`model-${model.value}`)}
            <div>
              <dt>{model.value || "Unspecified model"}</dt>
              <dd>{model.attempts.eligible} attempts</dd>
            </div>
          {/each}
        </dl>
      </Card>
    </div>
  {/if}
</section>

<style>
  .analytics-view {
    width: 100%;
    min-width: 0;
    overflow: auto;
    padding: clamp(1rem, 2.5vw, 2rem);
  }

  .analytics-header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--space-6);
    max-width: 92rem;
    margin: 0 auto var(--space-6);
  }

  .eyebrow {
    margin: 0 0 var(--space-2);
    color: var(--accent-blue);
    font-size: var(--font-size-xs);
    font-weight: 700;
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }

  h1,
  h2,
  p {
    margin-top: 0;
  }
  h1 {
    margin-bottom: var(--space-2);
    font-size: clamp(1.5rem, 3vw, 2.2rem);
  }
  .lede,
  .state-panel p {
    margin-bottom: 0;
    color: var(--text-secondary);
  }

  .filter-row {
    display: flex;
    max-width: 92rem;
    margin: 0 auto var(--space-5);
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .filter-row :global(.kit-select-dropdown) {
    min-width: 9.5rem;
  }

  .stale-notice {
    display: flex;
    max-width: 92rem;
    margin: 0 auto var(--space-5);
    padding: var(--space-3) var(--space-4);
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    border: 1px solid
      color-mix(in srgb, var(--accent-amber) 45%, var(--border-default));
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-amber) 8%, var(--bg-surface));
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .state-panel {
    display: grid;
    max-width: 48rem;
    min-height: 18rem;
    margin: 4rem auto;
    padding: var(--space-8);
    place-items: center;
    align-content: center;
    gap: var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    background: var(--bg-surface);
    text-align: center;
  }

  .state-panel h2 {
    margin-bottom: 0;
  }

  .metric-grid,
  .chart-grid,
  .breakdown-grid,
  .analytics-view > :global(.kit-card) {
    max-width: 92rem;
    margin-right: auto;
    margin-left: auto;
  }

  .metric-grid {
    display: grid;
    grid-template-columns: repeat(5, minmax(0, 1fr));
    gap: var(--space-4);
    margin-bottom: var(--space-5);
  }

  .chart-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-5);
    margin-bottom: var(--space-5);
  }

  .analytics-view > :global(.kit-card) {
    margin-bottom: var(--space-5);
  }

  .numeric {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  .breakdown-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--space-5);
  }

  .breakdown-list {
    display: grid;
    gap: var(--space-2);
    margin: 0;
  }
  .breakdown-list div {
    display: flex;
    justify-content: space-between;
    gap: var(--space-4);
  }
  .breakdown-list dt {
    color: var(--text-secondary);
  }
  .breakdown-list dd {
    margin: 0;
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  @media (max-width: 70rem) {
    .metric-grid {
      grid-template-columns: repeat(3, minmax(0, 1fr));
    }
    .breakdown-grid {
      grid-template-columns: 1fr 1fr;
    }
  }

  @media (max-width: 48rem) {
    .analytics-header {
      flex-direction: column;
    }
    .metric-grid,
    .chart-grid,
    .breakdown-grid {
      grid-template-columns: 1fr;
    }
    .filter-row :global(.kit-select-dropdown) {
      flex: 1 1 10rem;
    }
  }
</style>
