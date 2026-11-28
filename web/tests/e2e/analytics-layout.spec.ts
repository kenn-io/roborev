import { expect, test } from "@playwright/test";

import { openAnalytics } from "./support";

// Headless Chromium hides scrollbars by default, which would mask a
// regression back to classic layout-consuming scrollbars.
test.use({
  launchOptions: { ignoreDefaultArgs: ["--hide-scrollbars"] },
  viewport: { width: 1700, height: 700 },
});

test("keeps the centered analytics column in place when data makes the page scroll", async ({
  page,
}) => {
  await openAnalytics(page);
  let releaseAnalytics!: () => void;
  const analyticsReleased = new Promise<void>((resolve) => {
    releaseAnalytics = resolve;
  });
  await page.route("**/api/ui/analytics?**", async (route) => {
    await analyticsReleased;
    await route.continue();
  });

  await page.reload();
  await expect(page.getByText("Loading analytics…")).toBeVisible();
  const frame = page.locator(".analytics-frame");
  const view = frame.locator(".analytics-view");
  const heading = page.getByRole("heading", { name: "Review analytics" });
  expect(
    await view.evaluate(
      (element) => element.scrollHeight > element.clientHeight,
    ),
  ).toBe(false);
  const loadingBox = await heading.boundingBox();

  releaseAnalytics();
  await expect(
    page.getByRole("table", { name: "Project analytics" }),
  ).toBeVisible();
  await expect
    .poll(() =>
      view.evaluate((element) => element.scrollHeight > element.clientHeight),
    )
    .toBe(true);
  expect(
    await view.evaluate(
      (element) => (element as HTMLElement).offsetWidth - element.clientWidth,
    ),
  ).toBe(0);
  await view.hover();
  await expect(
    frame.locator('.kit-scrollbar--y[data-visible="true"]'),
  ).toBeVisible();
  const loadedBox = await heading.boundingBox();

  expect(loadedBox?.x).toBe(loadingBox?.x);
});
