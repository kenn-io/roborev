<script module lang="ts">
  import type { AnalyticsSummary } from "../../api/generated/models";

  export interface BreakdownRow {
    key: string;
    label: string;
    /** Chart color for this value, when the charts are split by it. */
    color?: string | undefined;
    summary: AnalyticsSummary;
  }
</script>

<script lang="ts">
  import {
    Table,
    TableHeaderCell,
    formatCost,
    formatDuration,
    formatNumber,
    type SortDirection,
  } from "@kenn-io/kit-ui";

  import {
    reviewLatency,
    verdictFailureRate,
  } from "../../utils/analytics-series";

  interface Props {
    ariaLabel: string;
    dimensionLabel: string;
    rows: ReadonlyArray<BreakdownRow>;
  }

  let { ariaLabel, dimensionLabel, rows }: Props = $props();

  interface Column {
    key: string;
    label: string;
    value: (row: BreakdownRow) => number | string | null;
    format: (row: BreakdownRow) => string;
  }

  const columns: Column[] = [
    {
      key: "reviews",
      label: "Reviews",
      value: (row) => row.summary.reviews.total,
      format: (row) => formatNumber(row.summary.reviews.total),
    },
    {
      key: "failure",
      label: "Failure rate",
      value: (row) => verdictFailureRate(row.summary),
      format: (row) => percentage(verdictFailureRate(row.summary)),
    },
    {
      key: "latency",
      label: "Median latency",
      value: (row) => reviewLatency(row.summary, "p50_secs"),
      format: (row) => duration(reviewLatency(row.summary, "p50_secs")),
    },
    {
      key: "attempts",
      label: "Agent runs",
      value: (row) => row.summary.attempts.eligible,
      format: (row) => formatNumber(row.summary.attempts.eligible),
    },
    {
      key: "cost",
      label: "Estimated cost",
      value: (row) => row.summary.cost.total_usd,
      format: (row) => formatCost(row.summary.cost.total_usd),
    },
    {
      key: "coverage",
      label: "Pricing coverage",
      value: (row) =>
        row.summary.cost.eligible_attempts > 0
          ? row.summary.cost.coverage
          : null,
      format: (row) =>
        percentage(
          row.summary.cost.eligible_attempts > 0
            ? row.summary.cost.coverage
            : null,
        ),
    },
  ];

  let sortKey = $state("reviews");
  let sortDirection = $state<SortDirection>("desc");

  const sortedRows = $derived.by(() => {
    const valueOf =
      sortKey === "name"
        ? (row: BreakdownRow) => row.label.toLowerCase()
        : (columns.find((column) => column.key === sortKey)?.value ??
          (() => null));
    const direction = sortDirection === "asc" ? 1 : -1;
    return [...rows].sort((a, b) => {
      const left = valueOf(a);
      const right = valueOf(b);
      // Rows without a value sort last in either direction.
      if (left === null || right === null) {
        return left === right ? 0 : left === null ? 1 : -1;
      }
      if (left < right) return -direction;
      if (left > right) return direction;
      return 0;
    });
  });

  function sortBy(key: string): void {
    if (sortKey === key) {
      sortDirection = sortDirection === "asc" ? "desc" : "asc";
      return;
    }
    sortKey = key;
    sortDirection = key === "name" ? "asc" : "desc";
  }

  function directionFor(key: string): SortDirection | null {
    return sortKey === key ? sortDirection : null;
  }

  function percentage(value: number | null): string {
    return value === null ? "–" : `${Math.round(value * 100)}%`;
  }

  function duration(seconds: number | null): string {
    return seconds === null ? "–" : formatDuration(seconds * 1000);
  }
</script>

<div class="breakdown">
  <Table {ariaLabel} stickyHeader={false}>
    {#snippet header()}
      <TableHeaderCell
        label={dimensionLabel}
        sortable
        sortDirection={directionFor("name")}
        onsort={() => sortBy("name")}
      />
      {#each columns as column (column.key)}
        <TableHeaderCell
          label={column.label}
          numeric
          sortable
          sortDirection={directionFor(column.key)}
          onsort={() => sortBy(column.key)}
        />
      {/each}
    {/snippet}
    {#each sortedRows as row (row.key)}
      <tr>
        <td class="name">
          {#if row.color}
            <span class="swatch" style:background={row.color}></span>
          {/if}
          {row.label}
        </td>
        {#each columns as column (column.key)}
          <td class="numeric">{column.format(row)}</td>
        {/each}
      </tr>
    {/each}
  </Table>
</div>

<style>
  .breakdown {
    overflow-x: auto;
  }

  .breakdown :global(.kit-th) {
    padding: 8px 16px;
    background: var(--bg-surface);
    font-weight: 500;
  }

  td {
    padding: 9px 16px;
    border-top: 1px solid var(--border-muted);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    white-space: nowrap;
  }

  tr:hover td {
    background: var(--bg-surface-hover);
  }

  .name {
    font-weight: 500;
  }

  .swatch {
    display: inline-block;
    width: 8px;
    height: 8px;
    margin-right: var(--space-4);
    border-radius: 2px;
    vertical-align: 0.05em;
  }

  .numeric {
    color: var(--text-secondary);
    font-variant-numeric: tabular-nums;
    text-align: right;
  }
</style>
