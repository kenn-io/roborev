import { expect, test } from "@playwright/test";

import { openAnalytics, openReviews } from "./support";

for (const initial of ["reviews", "analytics"] as const) {
  test(`reports app_opened and screen_viewed through the daemon from ${initial}`, async ({
    page,
  }) => {
    const reported = (event: string) =>
      page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          new URL(response.url()).pathname.endsWith("/api/telemetry/events") &&
          response.request().postDataJSON()?.event === event,
      );
    const appOpened = reported("app_opened");
    const screenViewed = reported("screen_viewed");
    if (initial === "reviews") await openReviews(page);
    else await openAnalytics(page);
    for (const [pending, event, properties] of [
      [appOpened, "app_opened", { surface: "web" }],
      [screenViewed, "screen_viewed", { screen: initial, surface: "web" }],
    ] as const) {
      const response = await pending;
      expect(response.status()).toBe(202);
      expect(await response.json()).toEqual({ status: "disabled" });
      expect(response.request().postDataJSON()).toEqual({ event, properties });
    }
  });
}
