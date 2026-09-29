import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it } from "vitest";

import AnalyticsChart from "./AnalyticsChart.svelte";

afterEach(cleanup);

const percent = (value: number) => `${Math.round(value * 100)}%`;
const dollars = (value: number) => `$${value.toFixed(2)}`;

describe("AnalyticsChart", () => {
  it("shows a fixed percentage axis and the hovered period's value", async () => {
    render(AnalyticsChart, {
      label: "Review failure rate over time",
      kind: "lines",
      periods: [
        { label: "Aug 1", partial: false },
        { label: "Aug 2", partial: false },
        { label: "Aug 3", partial: false },
      ],
      series: [
        {
          key: "all",
          label: "All reviews",
          color: "blue",
          values: [0.25, 0.42, 0.5],
        },
      ],
      formatValue: percent,
      maxValue: 1,
    });

    expect(screen.getByText("0%")).toBeTruthy();
    expect(screen.getByText("100%")).toBeTruthy();

    await fireEvent.pointerEnter(
      screen.getByRole("button", { name: "Aug 2: 42%" }),
    );

    const tooltip = screen.getByRole("tooltip").textContent ?? "";
    expect(tooltip).toContain("Aug 2");
    expect(tooltip).toContain("42%");
  });

  it("lists every series and the stacked total from the keyboard", async () => {
    render(AnalyticsChart, {
      label: "Estimated cost over time",
      kind: "bars",
      periods: [{ label: "Week of Aug 4", partial: true }],
      series: [
        { key: "a", label: "model-a", color: "blue", values: [1.25] },
        { key: "b", label: "model-b", color: "orange", values: [2.5] },
      ],
      formatValue: dollars,
    });

    const target = screen.getByRole("button", {
      name: "Week of Aug 4 (in progress): model-a $1.25, model-b $2.50",
    });
    await fireEvent.focus(target);

    const tooltip = screen.getByRole("tooltip").textContent ?? "";
    expect(tooltip).toContain("In progress");
    expect(tooltip).toContain("model-a");
    expect(tooltip).toContain("Total");
    expect(tooltip).toContain("$3.75");
  });

  it("reports periods without data instead of zero", () => {
    render(AnalyticsChart, {
      label: "Median review latency over time",
      kind: "lines",
      periods: [
        { label: "Aug 1", partial: false },
        { label: "Aug 2", partial: false },
      ],
      series: [{ key: "all", label: "All", color: "blue", values: [90, null] }],
      formatValue: (value: number) => `${value}s`,
    });

    expect(screen.getByRole("button", { name: "Aug 2: No data" })).toBeTruthy();
  });

  it("scales stacked bars to the column total", () => {
    render(AnalyticsChart, {
      label: "Logical reviews over time",
      kind: "bars",
      periods: [{ label: "Aug 1", partial: false }],
      series: [
        { key: "a", label: "a", color: "blue", values: [30] },
        { key: "b", label: "b", color: "orange", values: [25] },
      ],
      formatValue: (value: number) => `${value}`,
    });

    expect(screen.getByText("60")).toBeTruthy();
    expect(screen.queryByText("80")).toBeNull();
  });

  it("uses supplied tick steps for time axes", () => {
    render(AnalyticsChart, {
      label: "Median review latency over time",
      kind: "lines",
      periods: [{ label: "Aug 1", partial: false }],
      series: [{ key: "all", label: "All", color: "blue", values: [400] }],
      formatValue: (value: number) => `${value}s`,
      tickSteps: [60, 120, 300],
    });

    expect(screen.getByText("480s")).toBeTruthy();
    expect(screen.queryByText("500s")).toBeNull();
  });

  it("shows an empty state when no series has data", () => {
    render(AnalyticsChart, {
      label: "Estimated cost over time",
      kind: "bars",
      periods: [{ label: "Aug 1", partial: false }],
      series: [{ key: "all", label: "All", color: "blue", values: [null] }],
    });

    expect(screen.getByText("No data in this range.")).toBeTruthy();
    expect(screen.queryByRole("img")).toBeNull();
  });
});
