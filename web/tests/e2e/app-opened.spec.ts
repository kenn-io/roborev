import { expect, test, type Request } from "@playwright/test";

import { openReviews } from "./support";

const isTelemetryPost = (request: Request) =>
  request.method() === "POST" &&
  new URL(request.url()).pathname.endsWith("/api/telemetry/events");

test("receives a disabled app_opened receipt", async ({ page }) => {
  const appOpened = page.waitForResponse(
    (response) =>
      isTelemetryPost(response.request()) &&
      response.request().postDataJSON()?.event === "app_opened",
  );
  await openReviews(page);
  const response = await appOpened;
  expect(response.status()).toBe(202);
  expect(await response.json()).toEqual({ status: "disabled" });
});
