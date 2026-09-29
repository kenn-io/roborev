import type {
  AnalyticsSnapshot,
  AnalyticsSummary,
  AnalyticsTimeBucket,
} from "../api/generated/models";

// The categorical chart palette has eight slots (--series-1..8 in
// palette.css). The first eight values keep their own color, and the rest
// fold into one "Other" group drawn in the neutral --series-other.
const PALETTE_SLOTS = 8;

export const OTHER_KEY = "\u0000other";

export interface SeriesGroup {
  key: string;
  label: string;
  color: string;
  summary: AnalyticsSummary;
  buckets: AnalyticsTimeBucket[];
  /** Number of split values folded into this group (1 for named groups). */
  members: number;
}

export type LatencyPercentile = "p50_secs" | "p90_secs" | "p99_secs";

/**
 * Groups a snapshot's time series for charting. Without a split there is one
 * group for the whole selection. With a split, groups keep the daemon's
 * ranking (most reviews first), so colors follow that order and toggling a
 * group's visibility never recolors the others.
 */
export function seriesGroups(
  snapshot: AnalyticsSnapshot,
  labelFor: (value: string) => string,
): SeriesGroup[] {
  const split = snapshot.split_series ?? [];
  if (!snapshot.filters.split || split.length === 0) {
    return [
      {
        key: "all",
        label: "All reviews",
        color: "var(--series-1)",
        summary: snapshot.summary,
        buckets: snapshot.time_series ?? [],
        members: 1,
      },
    ];
  }
  const groups: SeriesGroup[] = split
    .slice(0, PALETTE_SLOTS)
    .map((series, i) => ({
      key: series.value,
      label: labelFor(series.value),
      color: `var(--series-${i + 1})`,
      summary: series.summary,
      buckets: series.time_series ?? [],
      members: 1,
    }));
  const rest = split.slice(PALETTE_SLOTS);
  if (rest.length > 0) {
    groups.push({
      key: OTHER_KEY,
      label: `Other (${rest.length})`,
      color: "var(--series-other)",
      summary: mergeSummaries(rest.map((series) => series.summary)),
      buckets: (rest[0]?.time_series ?? []).map((bucket, index) => ({
        start: bucket.start,
        end: bucket.end,
        ...mergeSummaries(
          rest.flatMap((series) => series.time_series?.[index] ?? []),
        ),
      })),
      members: rest.length,
    });
  }
  return groups;
}

/**
 * Sums the additive counts of several summaries and recomputes their rates.
 * Percentiles cannot be combined from summaries, so they are NaN; callers
 * must treat a merged group's latency as unknown.
 */
export function mergeSummaries(
  summaries: ReadonlyArray<AnalyticsSummary>,
): AnalyticsSummary {
  const unknown = { p50_secs: NaN, p90_secs: NaN, p99_secs: NaN };
  const total = {
    reviews: { total: 0, done: 0, failed: 0, canceled: 0, skipped: 0 },
    verdicts: { passed: 0, fail_open: 0, fail_closed: 0 },
    attempts: 0,
    cost: { total_usd: 0, eligible_attempts: 0, priced_attempts: 0 },
  };
  for (const summary of summaries) {
    total.reviews.total += summary.reviews.total;
    total.reviews.done += summary.reviews.done;
    total.reviews.failed += summary.reviews.failed;
    total.reviews.canceled += summary.reviews.canceled;
    total.reviews.skipped += summary.reviews.skipped;
    total.verdicts.passed += summary.verdicts.passed;
    total.verdicts.fail_open += summary.verdicts.fail_open;
    total.verdicts.fail_closed += summary.verdicts.fail_closed;
    total.attempts += summary.attempts.eligible;
    total.cost.total_usd += summary.cost.total_usd;
    total.cost.eligible_attempts += summary.cost.eligible_attempts;
    total.cost.priced_attempts += summary.cost.priced_attempts;
  }
  const runDenominator = total.reviews.done + total.reviews.failed;
  const runErrorRate =
    runDenominator > 0 ? total.reviews.failed / runDenominator : 0;
  const rated =
    total.verdicts.passed +
    total.verdicts.fail_open +
    total.verdicts.fail_closed;
  const failedVerdicts = total.verdicts.fail_open + total.verdicts.fail_closed;
  const { eligible_attempts: eligible, priced_attempts: priced } = total.cost;
  return {
    reviews: {
      ...total.reviews,
      failure_rate: runErrorRate,
      run_errors: total.reviews.failed,
      run_error_rate: runErrorRate,
    },
    verdicts: {
      ...total.verdicts,
      rated,
      failure_rate: rated > 0 ? failedVerdicts / rated : 0,
    },
    review_latency: unknown,
    attempts: { eligible: total.attempts, duration: unknown },
    cost: {
      ...total.cost,
      coverage: eligible > 0 ? priced / eligible : 0,
      complete: eligible > 0 && priced === eligible,
    },
  };
}

/** Failed verdicts over rated reviews, or null when nothing was rated. */
export function verdictFailureRate(summary: AnalyticsSummary): number | null {
  const { rated, fail_open, fail_closed } = summary.verdicts;
  return rated > 0 ? (fail_open + fail_closed) / rated : null;
}

/** Review latency percentile, or null when no review finished or unknown. */
export function reviewLatency(
  summary: AnalyticsSummary,
  percentile: LatencyPercentile,
): number | null {
  const value = summary.review_latency[percentile];
  return summary.reviews.done > 0 && Number.isFinite(value) ? value : null;
}
