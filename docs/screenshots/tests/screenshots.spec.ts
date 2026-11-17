import { expect, test } from "@playwright/test";
import { join } from "node:path";

import { findFirstFailingJob } from "./review-selection";

const outputDir = process.env.SCREENSHOT_DIR ?? "/output";

// Relative times such as "3d ago" are measured from this instant when set, so
// a corpus renders the same way on every capture date.
const screenshotNow = process.env.ROBOREV_SCREENSHOT_NOW;

test("review workspace with an open finding", async ({ page }) => {
  if (screenshotNow) {
    await page.clock.setFixedTime(new Date(screenshotNow));
  }
  await page.goto("/reviews");

  const jobs = page.getByRole("region", { name: "Review jobs" });
  await expect(jobs).toBeVisible();
  const failingJob = await findFirstFailingJob(page);
  await failingJob.click();

  await expect(
    page.getByRole("region", { name: "Review details" }),
  ).toBeVisible();
  await expect(page.locator(".review-content")).toBeVisible();
  await page.waitForTimeout(750);

  await page.screenshot({
    path: join(outputDir, "web-ui.png"),
    type: "png",
    animations: "disabled",
  });
});
