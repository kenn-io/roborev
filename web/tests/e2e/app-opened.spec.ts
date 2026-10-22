import { expect, test } from "@playwright/test";

import { openReviews } from "./support";

test("a disabled daemon returns a disabled browser receipt", async ({
  page,
}) => {
  const appOpened = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname.endsWith("/api/telemetry/events"),
  );
  await openReviews(page);
  const response = await appOpened;
  expect(response.status()).toBe(202);
  expect(await response.json()).toEqual({ status: "disabled" });
});
