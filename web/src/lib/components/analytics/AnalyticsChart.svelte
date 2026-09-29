<script module lang="ts">
  export interface ChartPeriod {
    label: string;
    /** The period extends past the snapshot's end, so its data is partial. */
    partial: boolean;
  }

  export interface ChartSeries {
    key: string;
    label: string;
    color: string;
    /** One value per period; null means no data (drawn as a gap). */
    values: ReadonlyArray<number | null>;
  }
</script>

<script lang="ts">
  interface Props {
    label: string;
    periods: ReadonlyArray<ChartPeriod>;
    series: ReadonlyArray<ChartSeries>;
    kind: "bars" | "lines";
    formatValue?: (value: number) => string;
    formatTick?: (value: number) => string;
    maxValue?: number;
    /** Candidate tick intervals, smallest first (for example, time units).
     * Without them the axis uses 1-2-5 steps. */
    tickSteps?: ReadonlyArray<number>;
    /** Series key to bring forward; the other series recede. */
    emphasis?: string | null;
  }

  let {
    label,
    periods,
    series,
    kind,
    formatValue = (value) => String(value),
    formatTick,
    maxValue,
    tickSteps,
    emphasis = null,
  }: Props = $props();

  const height = 220;
  const tickTarget = 4;
  const barMaxWidth = 24;
  const segmentGap = 2;

  let width = $state(0);
  let activeIndex = $state<number | null>(null);

  const stacked = $derived(kind === "bars");
  const columnTotals = $derived(
    periods.map((_, index) =>
      series.reduce((sum, item) => sum + (item.values[index] ?? 0), 0),
    ),
  );
  const dataMax = $derived(
    stacked
      ? Math.max(0, ...columnTotals)
      : Math.max(
          0,
          ...series.flatMap((item) =>
            item.values.filter((value): value is number => value !== null),
          ),
        ),
  );
  const scale = $derived(niceScale(maxValue ?? dataMax));
  const tickLabels = $derived(scale.ticks.map((tick) => tickLabel(tick)));
  const plot = $derived({
    top: 10,
    right: 8,
    bottom: 26,
    left: Math.max(28, ...tickLabels.map((text) => text.length * 6.4 + 10)),
  });
  const plotWidth = $derived(Math.max(0, width - plot.left - plot.right));
  const plotHeight = $derived(height - plot.top - plot.bottom);
  const slot = $derived(periods.length > 0 ? plotWidth / periods.length : 0);
  const barWidth = $derived(
    Math.max(2, Math.min(barMaxWidth, slot * 0.68 - 1)),
  );
  const xLabelIndexes = $derived(
    labelIndexes(periods.length, Math.max(2, Math.floor(plotWidth / 88))),
  );
  const hasData = $derived(
    series.some((item) => item.values.some((value) => value !== null)),
  );
  const active = $derived(
    activeIndex === null ? null : (periods[activeIndex] ?? null),
  );
  const tooltipOnLeft = $derived(
    activeIndex !== null && xFor(activeIndex) > width * 0.62,
  );

  function niceScale(maximum: number): { maximum: number; ticks: number[] } {
    if (!(maximum > 0)) return { maximum: 1, ticks: [0, 0.5, 1] };
    const rough = maximum / tickTarget;
    const power = 10 ** Math.floor(Math.log10(rough));
    const fraction = rough / power;
    const step =
      tickSteps?.find((candidate) => candidate >= rough) ??
      (fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10) * power;
    const intervals = Math.ceil(maximum / step - 1e-9);
    return {
      maximum: intervals * step,
      ticks: Array.from({ length: intervals + 1 }, (_, i) => i * step),
    };
  }

  function labelIndexes(length: number, count: number): Set<number> {
    if (length <= count) return new Set(Array.from({ length }, (_, i) => i));
    return new Set(
      Array.from({ length: count }, (_, i) =>
        Math.round((i * (length - 1)) / (count - 1)),
      ),
    );
  }

  function xFor(index: number): number {
    return plot.left + slot * (index + 0.5);
  }

  function yFor(value: number): number {
    const ratio = Math.max(0, Math.min(1, value / scale.maximum));
    return plot.top + (1 - ratio) * plotHeight;
  }

  function tickLabel(value: number): string {
    return (formatTick ?? formatValue)(value);
  }

  function display(value: number | null | undefined): string {
    return value === null || value === undefined
      ? "No data"
      : formatValue(value);
  }

  interface Segment {
    key: string;
    color: string;
    path: string;
  }

  // Stacked columns: the first series sits on the baseline. A 2px surface
  // gap separates segments, and only the topmost segment gets rounded
  // data-end corners.
  function columnSegments(index: number): Segment[] {
    const x = xFor(index) - barWidth / 2;
    const visible = series.filter((item) => (item.values[index] ?? 0) > 0);
    let base = 0;
    return visible.map((item, position) => {
      const value = item.values[index] ?? 0;
      const bottom = yFor(base) - (position > 0 ? segmentGap / 2 : 0);
      base += value;
      const top =
        yFor(base) + (position < visible.length - 1 ? segmentGap / 2 : 0);
      const segmentHeight = Math.max(1, bottom - top);
      const radius =
        position === visible.length - 1
          ? Math.min(4, barWidth / 2, segmentHeight)
          : 0;
      return {
        key: item.key,
        color: item.color,
        path: roundedTopRect(
          x,
          bottom - segmentHeight,
          barWidth,
          segmentHeight,
          radius,
        ),
      };
    });
  }

  function roundedTopRect(
    x: number,
    y: number,
    w: number,
    h: number,
    r: number,
  ): string {
    return (
      `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}` +
      `H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`
    );
  }

  // Lines break at periods without data instead of dropping to zero. A
  // partial final period joins the line with a dashed segment.
  function linePath(values: ReadonlyArray<number | null>): string {
    let path = "";
    let drawing = false;
    values.forEach((value, index) => {
      const partial = periods[index]?.partial ?? false;
      if (value === null || partial) {
        drawing = false;
        return;
      }
      path += `${drawing ? "L" : "M"}${xFor(index)},${yFor(value)}`;
      drawing = true;
    });
    return path;
  }

  function partialPath(values: ReadonlyArray<number | null>): string {
    const last = values.length - 1;
    const previous = values[last - 1];
    const current = values[last];
    if (
      !periods[last]?.partial ||
      previous === null ||
      previous === undefined ||
      current === null ||
      current === undefined
    ) {
      return "";
    }
    return `M${xFor(last - 1)},${yFor(previous)}L${xFor(last)},${yFor(current)}`;
  }

  function isolatedPoints(
    values: ReadonlyArray<number | null>,
  ): Array<{ index: number; value: number }> {
    const points: Array<{ index: number; value: number }> = [];
    values.forEach((value, index) => {
      if (value === null) return;
      const before = values[index - 1] ?? null;
      const after = values[index + 1] ?? null;
      if (before === null && after === null) points.push({ index, value });
    });
    return points;
  }

  function periodSummary(index: number): string {
    const period = periods[index];
    if (!period) return "";
    const suffix = period.partial ? " (in progress)" : "";
    if (series.length === 1) {
      return `${period.label}${suffix}: ${display(series[0]?.values[index])}`;
    }
    const parts = series.map(
      (item) => `${item.label} ${display(item.values[index])}`,
    );
    return `${period.label}${suffix}: ${parts.join(", ")}`;
  }
</script>

<div class="chart" bind:clientWidth={width}>
  {#if periods.length === 0 || !hasData}
    <p class="empty">No data in this range.</p>
  {:else}
    <div class="chart-stage" style:height={`${height}px`}>
      <svg {width} {height} role="img" aria-label={label}>
        {#each scale.ticks as tick, tickIndex (tick)}
          <line
            class="grid-line"
            class:baseline={tick === 0}
            x1={plot.left}
            x2={width - plot.right}
            y1={yFor(tick)}
            y2={yFor(tick)}
          ></line>
          <text class="y-label" x={plot.left - 8} y={yFor(tick) + 4}
            >{tickLabels[tickIndex]}</text
          >
        {/each}
        {#each periods as period, index (index)}
          {#if xLabelIndexes.has(index)}
            <text
              class="x-label"
              x={xFor(index)}
              y={height - 6}
              text-anchor={index === 0
                ? "start"
                : index === periods.length - 1
                  ? "end"
                  : "middle"}
              dx={index === 0
                ? -Math.min(slot / 2, 6)
                : index === periods.length - 1
                  ? Math.min(slot / 2, 6)
                  : 0}>{period.label}</text
            >
          {/if}
        {/each}

        {#if activeIndex !== null}
          <rect
            class="active-band"
            x={plot.left + slot * activeIndex}
            y={plot.top}
            width={slot}
            height={plotHeight}
          ></rect>
        {/if}

        {#if kind === "bars"}
          {#each periods as period, index (index)}
            <g class="column" class:partial={period.partial}>
              {#each columnSegments(index) as segment (segment.key)}
                <path
                  d={segment.path}
                  fill={segment.color}
                  class:recede={emphasis !== null && segment.key !== emphasis}
                ></path>
              {/each}
            </g>
          {/each}
        {:else}
          {#each series as item (item.key)}
            {@const recede = emphasis !== null && item.key !== emphasis}
            <path
              class="line"
              class:recede
              d={linePath(item.values)}
              stroke={item.color}
            ></path>
            <path
              class="line partial-line"
              class:recede
              d={partialPath(item.values)}
              stroke={item.color}
            ></path>
            {#each isolatedPoints(item.values) as point (point.index)}
              <circle
                class="point"
                class:recede
                cx={xFor(point.index)}
                cy={yFor(point.value)}
                r="3"
                fill={item.color}
              ></circle>
            {/each}
          {/each}
          {#if activeIndex !== null}
            {#each series as item (item.key)}
              {@const value = item.values[activeIndex]}
              {#if value !== null && value !== undefined}
                <circle
                  class="marker"
                  cx={xFor(activeIndex)}
                  cy={yFor(value)}
                  r="4"
                  fill={item.color}
                ></circle>
              {/if}
            {/each}
          {/if}
        {/if}
      </svg>

      <div
        class="targets"
        style:left={`${plot.left}px`}
        style:top={`${plot.top}px`}
        style:width={`${plotWidth}px`}
        style:height={`${plotHeight}px`}
      >
        {#each periods, index (index)}
          <button
            class="target"
            type="button"
            aria-label={periodSummary(index)}
            style:left={`${slot * index}px`}
            style:width={`${slot}px`}
            onpointerenter={() => (activeIndex = index)}
            onpointerleave={() => (activeIndex = null)}
            onfocus={() => (activeIndex = index)}
            onblur={() => (activeIndex = null)}
          ></button>
        {/each}
      </div>

      {#if active && activeIndex !== null}
        <div
          class="tooltip kit-popover-card"
          class:flip={tooltipOnLeft}
          role="tooltip"
          style:left={`${xFor(activeIndex)}px`}
        >
          <div class="tooltip-title">
            {active.label}
            {#if active.partial}<span class="tooltip-note">In progress</span
              >{/if}
          </div>
          {#each [...series].reverse() as item (item.key)}
            <div class="tooltip-row">
              {#if series.length > 1}
                <span class="swatch" style:background={item.color}></span>
                <span class="tooltip-label">{item.label}</span>
              {/if}
              <span class="tooltip-value"
                >{display(item.values[activeIndex])}</span
              >
            </div>
          {/each}
          {#if stacked && series.length > 1}
            <div class="tooltip-row tooltip-total">
              <span class="tooltip-label">Total</span>
              <span class="tooltip-value"
                >{formatValue(columnTotals[activeIndex] ?? 0)}</span
              >
            </div>
          {/if}
        </div>
      {/if}
    </div>

    <table class="accessible-values">
      <caption>{label}</caption>
      <thead>
        <tr>
          <th scope="col">Period</th>
          {#each series as item (item.key)}<th scope="col">{item.label}</th
            >{/each}
        </tr>
      </thead>
      <tbody>
        {#each periods as period, index (index)}
          <tr>
            <th scope="row"
              >{period.label}{period.partial ? " (in progress)" : ""}</th
            >
            {#each series as item (item.key)}
              <td>{display(item.values[index])}</td>
            {/each}
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

<style>
  .chart {
    position: relative;
    min-width: 0;
  }

  .chart-stage {
    position: relative;
  }

  svg {
    display: block;
    overflow: visible;
  }

  .grid-line {
    stroke: var(--border-muted);
    stroke-width: 1;
    shape-rendering: crispEdges;
  }

  .grid-line.baseline {
    stroke: var(--border-default);
  }

  text {
    fill: var(--text-muted);
    font-family: var(--font-sans);
    font-size: 11px;
    font-variant-numeric: tabular-nums;
  }

  .y-label {
    text-anchor: end;
  }

  .active-band {
    fill: var(--bg-surface-hover);
  }

  .column.partial {
    opacity: 0.45;
  }

  path,
  circle {
    transition: opacity var(--transition-fast);
  }

  .recede {
    opacity: 0.15;
  }

  .column path.recede {
    opacity: 0.25;
  }

  .line {
    fill: none;
    stroke-width: 2;
    stroke-linecap: round;
    stroke-linejoin: round;
  }

  .partial-line {
    stroke-dasharray: 3 4;
  }

  .marker {
    stroke: var(--bg-surface);
    stroke-width: 2;
  }

  .targets {
    position: absolute;
  }

  .target {
    position: absolute;
    top: 0;
    bottom: 0;
    padding: 0;
    border: 0;
    background: transparent;
    cursor: crosshair;
  }

  .target:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }

  .tooltip {
    position: absolute;
    top: 0;
    z-index: 2;
    display: grid;
    min-width: 9rem;
    padding: var(--space-4) var(--space-5);
    gap: var(--space-2);
    font-size: var(--font-size-xs);
    pointer-events: none;
    transform: translateX(12px);
  }

  .tooltip.flip {
    transform: translateX(calc(-100% - 12px));
  }

  .tooltip-title {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-5);
    margin-bottom: var(--space-1);
    color: var(--text-primary);
    font-weight: 600;
    white-space: nowrap;
  }

  .tooltip-note {
    color: var(--text-muted);
    font-weight: 400;
  }

  .tooltip-row {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .tooltip-label {
    overflow: hidden;
    max-width: 14rem;
    text-overflow: ellipsis;
  }

  .tooltip-value {
    margin-left: auto;
    padding-left: var(--space-5);
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .tooltip-total {
    margin-top: var(--space-1);
    padding-top: var(--space-3);
    border-top: 1px solid var(--border-muted);
  }

  .swatch {
    width: 8px;
    height: 8px;
    flex-shrink: 0;
    border-radius: 2px;
  }

  .empty {
    display: grid;
    min-height: 220px;
    margin: 0;
    place-items: center;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .accessible-values {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip-path: inset(50%);
    white-space: nowrap;
  }
</style>
