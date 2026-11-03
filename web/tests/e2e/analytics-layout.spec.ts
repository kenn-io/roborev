import { expect, test } from "@playwright/test";

import { openAnalytics } from "./support";

// Headless Chromium hides scrollbars by default, which would hide the
// scrollbar-width shift this test guards against.
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
  const view = page.locator(".analytics-view");
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
  expect(
    await view.evaluate(
      (element) => (element as HTMLElement).offsetWidth - element.clientWidth,
    ),
  ).toBeGreaterThan(0);
  const loadedBox = await heading.boundingBox();

  expect(loadedBox?.x).toBe(loadingBox?.x);
});
