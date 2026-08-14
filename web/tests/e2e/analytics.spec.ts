import { expect, test } from "@playwright/test";

import { openAnalytics } from "./support";

test.describe("native analytics workspace", () => {
  test("renders SQLite review health and incomplete cost coverage", async ({
    page,
  }) => {
    await openAnalytics(page);

    await expect(
      page.getByRole("img", { name: "Estimated cost over time" }),
    ).toBeVisible();
    await expect(
      page.getByRole("img", { name: "Review failure rate over time" }),
    ).toBeVisible();
    await expect(
      page.getByRole("img", { name: "Median review latency over time" }),
    ).toBeVisible();
    await expect(
      page.getByRole("cell", { name: "project-alpha" }),
    ).toBeVisible();
    await expect(
      page.getByRole("cell", { name: "project-beta" }),
    ).toBeVisible();
    await expect(page.getByText(/coverage$/).first()).toBeVisible();
    await expect(
      page.getByText("Estimated cost is a lower bound"),
    ).toBeVisible();
    await expect(
      page.getByText("8 failed verdicts of 27 rated reviews").first(),
    ).toBeVisible();
    await expect(
      page.getByText("6 run errors · 6 canceled · 5 skipped"),
    ).toBeVisible();
    await expect(page.getByText("0%", { exact: true }).first()).toBeVisible();
    await expect(page.getByText("100%", { exact: true }).first()).toBeVisible();

    const failureChart = page.locator(".chart").filter({
      has: page.getByRole("img", { name: "Review failure rate over time" }),
    });
    const point = failureChart.getByRole("button").first();
    await point.hover();
    await expect(failureChart.getByRole("tooltip")).toBeVisible();
    await expect(failureChart.getByRole("tooltip")).toContainText(/\d+%/);
  });

  test("keeps filters in history and never shows old project data", async ({
    page,
  }) => {
    await openAnalytics(page);

    await page.getByRole("button", { name: "Projects" }).click();
    await page
      .locator(".kit-filter-dropdown__item", { hasText: "project-beta" })
      .click();

    await expect(page).toHaveURL(/project=project-beta/);
    await expect(
      page.getByRole("cell", { name: "project-beta" }),
    ).toBeVisible();
    await expect(page.getByRole("cell", { name: "project-alpha" })).toHaveCount(
      0,
    );

    await page.goBack();
    await expect(page).not.toHaveURL(/project=project-beta/);
    await expect(
      page.getByRole("cell", { name: "project-alpha" }),
    ).toBeVisible();
  });

  test("sends Manual as an explicit empty source filter", async ({ page }) => {
    await openAnalytics(page);
    const filteredRequest = page.waitForRequest((request) => {
      const url = new URL(request.url());
      return (
        url.pathname === "/api/ui/analytics" && url.searchParams.has("source")
      );
    });

    await page.getByRole("button", { name: "Sources" }).click();
    await page
      .locator(".kit-filter-dropdown__item", { hasText: "Manual" })
      .click();

    const requestURL = new URL((await filteredRequest).url());
    expect(requestURL.searchParams.getAll("source")).toEqual([""]);
    await expect(page).toHaveURL(/source=/);
    await expect(page.getByText("Manual").last()).toBeVisible();
  });

  test("retains the current snapshot when a manual refresh fails", async ({
    page,
  }) => {
    await openAnalytics(page);
    await expect(
      page.getByRole("cell", { name: "project-alpha" }),
    ).toBeVisible();
    await page.route("**/api/ui/analytics?**", (route) =>
      route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ detail: "temporarily unavailable" }),
      }),
    );

    await page.getByRole("button", { name: "Refresh analytics" }).click();

    await expect(page.getByRole("status")).toContainText("Showing stale data");
    await expect(
      page.getByRole("cell", { name: "project-alpha" }),
    ).toBeVisible();
  });
});
