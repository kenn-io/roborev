import { expect, test } from "@playwright/test";

import { openReviews } from "./support";

test("reports app_opened through the daemon when the web UI loads", async ({
  page,
}) => {
  const reported = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname.endsWith("/api/telemetry/events"),
  );
  await openReviews(page);
  const response = await reported;

  expect(response.status()).toBe(202);
  expect(await response.json()).toEqual({ status: "disabled" });
  expect(response.request().postDataJSON()).toEqual({ event: "app_opened" });
});
