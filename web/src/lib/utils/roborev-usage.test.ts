import { describe, expect, it } from "vitest";
import {
  formatTokenCount,
  parseCostUsd,
  parseTokenUsage,
  tokenUsageDetail,
  tokenUsageStats,
} from "./roborev-usage";

describe("parseTokenUsage", () => {
  it("reads the fields a session usage blob reports", () => {
    const usage = parseTokenUsage(
      JSON.stringify({
        input_tokens: 231582,
        cached_input_tokens: 189952,
        total_output_tokens: 2542,
        peak_context_tokens: 47248,
        cost_usd: 0.347212,
        has_cost: true,
      }),
    );

    expect(usage).toEqual({
      inputTokens: 231582,
      cachedInputTokens: 189952,
      outputTokens: 2542,
      peakContextTokens: 47248,
      costUsd: 0.347212,
    });
  });

  it("returns null for missing, unparseable, and empty blobs", () => {
    expect(parseTokenUsage(undefined)).toBeNull();
    expect(parseTokenUsage("")).toBeNull();
    expect(parseTokenUsage("12k tokens")).toBeNull();
    expect(parseTokenUsage("null")).toBeNull();
    expect(parseTokenUsage("{}")).toBeNull();
    expect(parseTokenUsage(JSON.stringify({ session_id: "abc" }))).toBeNull();
  });

  it("ignores cost when the blob reports no cost data", () => {
    const usage = parseTokenUsage(
      JSON.stringify({
        total_output_tokens: 100,
        cost_usd: 5,
        has_cost: false,
      }),
    );
    expect(usage).toEqual({
      inputTokens: null,
      cachedInputTokens: null,
      outputTokens: 100,
      peakContextTokens: null,
      costUsd: null,
    });
  });
});

describe("parseCostUsd", () => {
  it("extracts priced cost and stays null otherwise", () => {
    expect(
      parseCostUsd(JSON.stringify({ cost_usd: 0.42, has_cost: true })),
    ).toBe(0.42);
    expect(
      parseCostUsd(JSON.stringify({ cost_usd: 0.42, has_cost: false })),
    ).toBeNull();
    expect(
      parseCostUsd(JSON.stringify({ cost_usd: "0.42", has_cost: true })),
    ).toBeNull();
    expect(parseCostUsd(undefined)).toBeNull();
  });
});

describe("formatTokenCount", () => {
  it("abbreviates counts by magnitude", () => {
    expect(formatTokenCount(0)).toBe("0");
    expect(formatTokenCount(942)).toBe("942");
    expect(formatTokenCount(2542)).toBe("2.5k");
    expect(formatTokenCount(47248)).toBe("47k");
    expect(formatTokenCount(231582)).toBe("232k");
    expect(formatTokenCount(1_250_000)).toBe("1.3M");
  });
});

describe("tokenUsageStats", () => {
  it("emits only the stats the blob reported", () => {
    const usage = parseTokenUsage(
      JSON.stringify({ total_output_tokens: 2542, peak_context_tokens: 47248 }),
    );
    expect(usage).not.toBeNull();
    expect(tokenUsageStats(usage!)).toEqual([
      { label: "out", value: "2.5k" },
      { label: "peak", value: "47k" },
    ]);
  });
});

describe("tokenUsageDetail", () => {
  it("keeps full precision for the hover text", () => {
    const usage = parseTokenUsage(
      JSON.stringify({
        input_tokens: 231582,
        cached_input_tokens: 189952,
        total_output_tokens: 2542,
        peak_context_tokens: 47248,
        cost_usd: 0.347212,
        has_cost: true,
      }),
    );
    expect(tokenUsageDetail(usage!)).toBe(
      "input 231,582 · cached input 189,952 · output 2,542 · peak context 47,248 · cost $0.347212",
    );
  });

  it("keeps sub-cent costs legible instead of rounding them to zero", () => {
    const usage = parseTokenUsage(
      JSON.stringify({ cost_usd: 0.000023, has_cost: true }),
    );
    expect(tokenUsageDetail(usage!)).toBe("cost $0.000023");
  });
});
