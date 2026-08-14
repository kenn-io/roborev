import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it } from "vitest";

import TimeSeriesChart from "./TimeSeriesChart.svelte";

afterEach(cleanup);

describe("TimeSeriesChart", () => {
  it("shows a fixed percentage axis and exact pointer tooltip", async () => {
    render(TimeSeriesChart, {
      label: "Review failure rate over time",
      points: [
        { label: "Aug 1", value: 0.25 },
        { label: "Aug 2", value: 0.42 },
        { label: "Aug 3", value: 0.5 },
      ],
      formatValue: (value: number) => `${Math.round(value * 100)}%`,
      formatTick: (value: number) => `${Math.round(value * 100)}%`,
      minValue: 0,
      maxValue: 1,
    });

    expect(screen.getByText("0%")).toBeTruthy();
    expect(screen.getByText("100%")).toBeTruthy();
    expect(screen.getAllByText("Aug 1").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Aug 3").length).toBeGreaterThan(0);

    const point = screen.getByRole("button", { name: "Aug 2: 42%" });
    await fireEvent.pointerEnter(point);

    expect(screen.getByRole("tooltip").textContent).toContain("Aug 2");
    expect(screen.getByRole("tooltip").textContent).toContain("42%");
  });

  it("exposes exact values from the keyboard", async () => {
    render(TimeSeriesChart, {
      label: "Estimated cost over time",
      points: [{ label: "Week of Aug 4", value: 12.34 }],
      formatValue: (value: number) => `$${value.toFixed(2)}`,
      formatTick: (value: number) => `$${Math.round(value)}`,
    });

    const point = screen.getByRole("button", {
      name: "Week of Aug 4: $12.34",
    });
    await fireEvent.focus(point);

    expect(screen.getByRole("tooltip").textContent).toContain("Week of Aug 4");
    expect(screen.getByRole("tooltip").textContent).toContain("$12.34");
  });

  it("chooses ticks from the interval instead of doubling the data range", () => {
    render(TimeSeriesChart, {
      label: "Estimated cost over time",
      points: [
        { label: "Aug 1", value: 120 },
        { label: "Aug 2", value: 260.82 },
      ],
      formatValue: (value: number) => `$${value.toFixed(2)}`,
      formatTick: (value: number) => `$${Math.round(value)}`,
    });

    expect(screen.getByText("$300")).toBeTruthy();
    expect(screen.queryByText("$500")).toBeNull();
  });
});
