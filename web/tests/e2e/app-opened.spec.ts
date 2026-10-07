import { expect, test } from "@playwright/test";

import { openAnalytics, openReviews } from "./support";

test("reports app_opened through the daemon when the web UI loads", async ({
  page,
}) => {
  const reported = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname.endsWith("/api/telemetry/events") &&
      response.request().postDataJSON()?.event === "app_opened",
  );
  await openReviews(page);
  const response = await reported;

  expect(response.status()).toBe(202);
  expect(await response.json()).toEqual({ status: "disabled" });
  expect(response.request().postDataJSON()).toEqual({
    event: "app_opened",
    properties: { surface: "web" },
  });
});

for (const initial of ["reviews", "analytics"] as const) {
  test(`reports screen_viewed from ${initial} and on navigation`, async ({
    page,
  }) => {
    const screenResponse = (screen: string) =>
      page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          new URL(response.url()).pathname.endsWith("/api/telemetry/events") &&
          response.request().postDataJSON()?.event === "screen_viewed" &&
          response.request().postDataJSON()?.properties.screen === screen,
      );
    const first = screenResponse(initial);
    if (initial === "reviews") await openReviews(page);
    else await openAnalytics(page);
    const response = await first;
    expect(response.status()).toBe(202);
    expect(await response.json()).toEqual({ status: "disabled" });
    const other = initial === "reviews" ? "analytics" : "reviews";
    const second = screenResponse(other);
    await page
      .getByRole("navigation", { name: "Application" })
      .getByRole("button", {
        name: other === "reviews" ? "Reviews" : "Analytics",
        exact: true,
      })
      .click();
    expect((await second).status()).toBe(202);
    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`/${initial}`));
  });
}
