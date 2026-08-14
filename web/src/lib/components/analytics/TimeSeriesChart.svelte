<script lang="ts">
  interface Point {
    label: string;
    value: number;
  }

  interface Props {
    label: string;
    points: Point[];
    formatValue?: (value: number) => string;
    formatTick?: (value: number) => string;
    minValue?: number;
    maxValue?: number;
    tone?: "blue" | "green" | "amber" | "red";
  }

  let {
    label,
    points,
    formatValue = (value) => String(value),
    formatTick,
    minValue = 0,
    maxValue,
    tone = "blue",
  }: Props = $props();

  const width = 600;
  const height = 220;
  const plot = { top: 12, right: 12, bottom: 34, left: 58 };
  const plotWidth = width - plot.left - plot.right;
  const plotHeight = height - plot.top - plot.bottom;
  const tickCount = 4;

  let activeIndex = $state<number | null>(null);
  const yScale = $derived(resolveScale(points, minValue, maxValue));
  const domainMax = $derived(yScale.maximum);
  const yTicks = $derived(yScale.ticks);
  const xLabelIndexes = $derived(axisLabelIndexes(points.length));
  const polyline = $derived(
    points
      .map(
        (point, index) =>
          `${xFor(index, points.length).toFixed(1)},${yFor(point.value, minValue, domainMax).toFixed(1)}`,
      )
      .join(" "),
  );
  const activePoint = $derived(
    activeIndex === null ? null : (points[activeIndex] ?? null),
  );
  const activeX = $derived(
    activeIndex === null ? 0 : xFor(activeIndex, points.length),
  );
  const activeY = $derived(
    activePoint === null ? 0 : yFor(activePoint.value, minValue, domainMax),
  );

  function resolveScale(
    values: Point[],
    minimum: number,
    explicitMaximum: number | undefined,
  ): { maximum: number; ticks: number[] } {
    if (explicitMaximum !== undefined) {
      const maximum = explicitMaximum > minimum ? explicitMaximum : minimum + 1;
      return {
        maximum,
        ticks: Array.from(
          { length: tickCount + 1 },
          (_, index) => minimum + ((maximum - minimum) * index) / tickCount,
        ),
      };
    }
    const dataMaximum = Math.max(
      minimum,
      ...values.map((point) =>
        Number.isFinite(point.value) ? point.value : minimum,
      ),
    );
    const distance = dataMaximum - minimum;
    if (distance <= 0) {
      return resolveScale(values, minimum, minimum + 1);
    }
    const step = niceTickStep(distance / tickCount);
    const intervals = Math.ceil(distance / step);
    return {
      maximum: minimum + intervals * step,
      ticks: Array.from(
        { length: intervals + 1 },
        (_, index) => minimum + index * step,
      ),
    };
  }

  function niceTickStep(value: number): number {
    const power = 10 ** Math.floor(Math.log10(value));
    const fraction = value / power;
    const rounded =
      fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10;
    return rounded * power;
  }

  function xFor(index: number, length: number): number {
    return length === 1
      ? plot.left + plotWidth / 2
      : plot.left + (index / (length - 1)) * plotWidth;
  }

  function yFor(value: number, minimum: number, maximum: number): number {
    const ratio = Math.max(
      0,
      Math.min(1, (value - minimum) / (maximum - minimum)),
    );
    return plot.top + (1 - ratio) * plotHeight;
  }

  function axisLabelIndexes(length: number): Set<number> {
    if (length <= 5) {
      return new Set(Array.from({ length }, (_, index) => index));
    }
    return new Set(
      Array.from({ length: 5 }, (_, index) =>
        Math.round((index * (length - 1)) / 4),
      ),
    );
  }

  function tooltipLeft(x: number): number {
    return Math.max(12, Math.min(88, (x / width) * 100));
  }

  function tickLabel(value: number): string {
    return (formatTick ?? formatValue)(value);
  }
</script>

<div class="chart" data-tone={tone}>
  {#if points.length === 0}
    <p class="empty">No values in this range.</p>
  {:else}
    <div class="chart-stage">
      <svg viewBox={`0 0 ${width} ${height}`} role="img" aria-label={label}>
        {#each yTicks as tick (`y-${tick}`)}
          {@const y = yFor(tick, minValue, domainMax)}
          <line
            class="grid-line"
            x1={plot.left}
            y1={y}
            x2={width - plot.right}
            y2={y}
          ></line>
          <text class="y-label" x={plot.left - 9} y={y + 4}
            >{tickLabel(tick)}</text
          >
        {/each}
        {#each points as point, index (`x-${point.label}-${index}`)}
          {#if xLabelIndexes.has(index)}
            {@const x = xFor(index, points.length)}
            <text
              class="x-label"
              {x}
              y={height - 8}
              text-anchor={index === 0
                ? "start"
                : index === points.length - 1
                  ? "end"
                  : "middle"}>{point.label}</text
            >
          {/if}
        {/each}
        <polyline points={polyline}></polyline>
        {#if activePoint}
          <line
            class="crosshair"
            x1={activeX}
            y1={plot.top}
            x2={activeX}
            y2={height - plot.bottom}
          ></line>
        {/if}
        {#each points as point, index (`point-${point.label}-${index}`)}
          <circle
            class:active={activeIndex === index}
            cx={xFor(index, points.length)}
            cy={yFor(point.value, minValue, domainMax)}
            r={activeIndex === index ? 5 : 3}
          ></circle>
        {/each}
      </svg>
      {#each points as point, index (`target-${point.label}-${index}`)}
        <button
          class="point-target"
          type="button"
          aria-label={`${point.label}: ${formatValue(point.value)}`}
          style={`left: ${(xFor(index, points.length) / width) * 100}%; top: ${(yFor(point.value, minValue, domainMax) / height) * 100}%`}
          onpointerenter={() => (activeIndex = index)}
          onpointerleave={() => (activeIndex = null)}
          onfocus={() => (activeIndex = index)}
          onblur={() => (activeIndex = null)}
        ></button>
      {/each}
      {#if activePoint}
        <div
          class="tooltip"
          role="tooltip"
          style={`left: ${tooltipLeft(activeX)}%; top: ${(activeY / height) * 100}%`}
        >
          <strong>{activePoint.label}</strong>
          <span>{formatValue(activePoint.value)}</span>
        </div>
      {/if}
    </div>
    <table class="accessible-values">
      <caption>{label}</caption>
      <thead><tr><th>Period</th><th>Value</th></tr></thead>
      <tbody>
        {#each points as point, index (`table-${point.label}-${index}`)}
          <tr><td>{point.label}</td><td>{formatValue(point.value)}</td></tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

<style>
  .chart {
    min-height: 13.75rem;
  }

  .chart-stage {
    position: relative;
    width: 100%;
    aspect-ratio: 600 / 220;
  }

  svg {
    display: block;
    width: 100%;
    height: 100%;
    overflow: visible;
  }

  .grid-line {
    stroke: var(--border-muted);
    stroke-width: 1;
    vector-effect: non-scaling-stroke;
  }

  text {
    fill: var(--text-muted);
    font-family: inherit;
    font-size: 11px;
  }

  .y-label {
    text-anchor: end;
  }

  polyline {
    fill: none;
    stroke: var(--chart-color, var(--accent-blue));
    stroke-width: 3;
    stroke-linecap: round;
    stroke-linejoin: round;
    vector-effect: non-scaling-stroke;
  }

  circle {
    fill: var(--bg-surface);
    stroke: var(--chart-color, var(--accent-blue));
    stroke-width: 2;
    vector-effect: non-scaling-stroke;
  }

  circle.active {
    fill: var(--chart-color, var(--accent-blue));
  }

  .crosshair {
    stroke: var(--text-muted);
    stroke-width: 1;
    stroke-dasharray: 3 3;
    vector-effect: non-scaling-stroke;
  }

  .point-target {
    position: absolute;
    width: 2rem;
    height: 2rem;
    padding: 0;
    border: 0;
    border-radius: 50%;
    background: transparent;
    cursor: crosshair;
    transform: translate(-50%, -50%);
  }

  .point-target:focus-visible {
    outline: 2px solid var(--focus-ring, var(--accent-blue));
    outline-offset: 2px;
  }

  .tooltip {
    position: absolute;
    z-index: 1;
    display: grid;
    min-width: max-content;
    padding: var(--space-2) var(--space-3);
    gap: 0.1rem;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-elevated, var(--bg-surface));
    box-shadow: var(--shadow-md);
    color: var(--text-primary);
    font-size: var(--font-size-xs);
    pointer-events: none;
    transform: translate(-50%, calc(-100% - 0.7rem));
  }

  .tooltip span {
    color: var(--text-secondary);
    font-variant-numeric: tabular-nums;
  }

  [data-tone="green"] {
    --chart-color: var(--accent-green);
  }
  [data-tone="amber"] {
    --chart-color: var(--accent-amber);
  }
  [data-tone="red"] {
    --chart-color: var(--accent-red);
  }

  .empty {
    display: grid;
    min-height: 13.75rem;
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
    clip: rect(0 0 0 0);
    clip-path: inset(50%);
    white-space: nowrap;
  }
</style>
