import { expect, test, type Request } from "@playwright/test";

import { openAnalytics, openReviews } from "./support";

const isTelemetryPost = (request: Request) =>
  request.method() === "POST" &&
  new URL(request.url()).pathname.endsWith("/api/telemetry/events");

for (const initial of ["reviews", "analytics"] as const) {
  test(`quiets screen and session reports after a disabled receipt from ${initial}`, async ({
    page,
  }) => {
    const appOpened = page.waitForResponse(
      (response) =>
        isTelemetryPost(response.request()) &&
        response.request().postDataJSON()?.event === "app_opened",
    );
    if (initial === "reviews") await openReviews(page);
    else await openAnalytics(page);
    const response = await appOpened;
    expect(response.status()).toBe(202);
    expect(await response.json()).toEqual({ status: "disabled" });

    await expect(
      page.waitForRequest(isTelemetryPost, { timeout: 250 }),
    ).rejects.toThrow(/Timeout/);
  });
}
