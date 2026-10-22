import { expect, test } from "@playwright/test";

import { openAnalytics, openReviews } from "./support";

for (const initial of ["reviews", "analytics"] as const) {
  test(`a disabled daemon quiets browser reports from ${initial}`, async ({
    page,
  }) => {
    const events: unknown[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "POST" &&
        new URL(request.url()).pathname.endsWith("/api/telemetry/events")
      )
        events.push(request.postDataJSON());
    });
    const appOpened = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname.endsWith("/api/telemetry/events"),
    );
    if (initial === "reviews") await openReviews(page);
    else await openAnalytics(page);
    const response = await appOpened;
    expect(response.status()).toBe(202);
    expect(await response.json()).toEqual({ status: "disabled" });
    await page.evaluate(() => globalThis.dispatchEvent(new Event("pagehide")));
    await page.reload();
    if (initial === "reviews")
      await expect(page.locator(".job-row").first()).toBeVisible();
    else
      await expect(
        page.getByRole("table", { name: "Project analytics" }),
      ).toBeVisible();
    expect(events).toEqual([
      { event: "app_opened", properties: { surface: "web" } },
    ]);
  });
}
