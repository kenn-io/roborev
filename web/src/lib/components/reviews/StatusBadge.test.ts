import { cleanup, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it } from "vitest";

import StatusBadge from "./StatusBadge.svelte";

describe("StatusBadge", () => {
  afterEach(() => {
    cleanup();
  });

  it("renders review status through the shared dotted chip semantics", () => {
    render(StatusBadge, {
      props: {
        status: "running",
      },
    });

    const chip = screen.getByText("running").closest(".kit-chip");
    expect(chip).not.toBeNull();
    expect(chip?.classList.contains("kit-chip--tone-info")).toBe(true);
    expect(chip?.classList.contains("status-badge")).toBe(true);
    expect(chip?.querySelector(".kit-chip__dot")).not.toBeNull();
  });

  it("keeps canceled reviews visually distinct from unknown statuses", () => {
    render(StatusBadge, {
      props: {
        status: "canceled",
      },
    });

    const chip = screen.getByText("canceled").closest(".kit-chip");
    expect(chip?.classList.contains("kit-chip--tone-canceled")).toBe(true);
    expect(chip?.classList.contains("status-canceled")).toBe(true);
  });
});
