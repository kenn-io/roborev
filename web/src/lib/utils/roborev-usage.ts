// Parses a roborev job's token_usage JSON blob into the fields the UI shows.
// The blob is a serialized session usage payload; it is never rendered raw.

export interface JobTokenUsage {
  inputTokens: number | null;
  cachedInputTokens: number | null;
  outputTokens: number | null;
  peakContextTokens: number | null;
  costUsd: number | null;
}

export interface TokenUsageStat {
  label: string;
  value: string;
}

export function parseTokenUsage(
  tokenUsage: string | undefined,
): JobTokenUsage | null {
  if (!tokenUsage) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(tokenUsage);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const record = parsed as Record<string, unknown>;
  const num = (key: string): number | null => {
    const value = record[key];
    return typeof value === "number" && Number.isFinite(value) ? value : null;
  };
  const usage: JobTokenUsage = {
    inputTokens: num("input_tokens"),
    cachedInputTokens: num("cached_input_tokens"),
    outputTokens: num("total_output_tokens"),
    peakContextTokens: num("peak_context_tokens"),
    costUsd: record.has_cost === true ? num("cost_usd") : null,
  };
  const hasAnyField = Object.values(usage).some((value) => value !== null);
  return hasAnyField ? usage : null;
}

// Extracts the USD cost from a job's token_usage blob.
// Returns null when the job has no reported cost.
export function parseCostUsd(tokenUsage: string | undefined): number | null {
  return parseTokenUsage(tokenUsage)?.costUsd ?? null;
}

export function formatTokenCount(value: number): string {
  const abs = Math.abs(value);
  if (abs >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (abs >= 10_000) return `${Math.round(value / 1000)}k`;
  if (abs >= 1000) return `${(value / 1000).toFixed(1)}k`;
  return `${value}`;
}

// Compact footer stats: short labels with abbreviated counts.
export function tokenUsageStats(usage: JobTokenUsage): TokenUsageStat[] {
  const stats: TokenUsageStat[] = [];
  if (usage.costUsd !== null) {
    stats.push({ label: "cost", value: `~$${usage.costUsd.toFixed(2)}` });
  }
  if (usage.inputTokens !== null) {
    stats.push({ label: "in", value: formatTokenCount(usage.inputTokens) });
  }
  if (usage.outputTokens !== null) {
    stats.push({ label: "out", value: formatTokenCount(usage.outputTokens) });
  }
  if (usage.peakContextTokens !== null) {
    stats.push({
      label: "peak",
      value: formatTokenCount(usage.peakContextTokens),
    });
  }
  return stats;
}

// Full-precision hover text for the compact stats.
export function tokenUsageDetail(usage: JobTokenUsage): string {
  const group = (value: number): string => value.toLocaleString("en-US");
  const parts: string[] = [];
  if (usage.inputTokens !== null)
    parts.push(`input ${group(usage.inputTokens)}`);
  if (usage.cachedInputTokens !== null) {
    parts.push(`cached input ${group(usage.cachedInputTokens)}`);
  }
  if (usage.outputTokens !== null)
    parts.push(`output ${group(usage.outputTokens)}`);
  if (usage.peakContextTokens !== null) {
    parts.push(`peak context ${group(usage.peakContextTokens)}`);
  }
  if (usage.costUsd !== null) {
    // Significant digits rather than fixed decimals, so a sub-cent job reads as
    // $0.000023 instead of toFixed(4)'s $0.0000. Six of them, not the raw
    // double: that is finer than any real per-job cost, and String() would
    // surface float artifacts ($0.30000000000000004) or exponent notation.
    const cost = usage.costUsd.toLocaleString("en-US", {
      maximumSignificantDigits: 6,
    });
    parts.push(`cost $${cost}`);
  }
  return parts.join(" · ");
}
