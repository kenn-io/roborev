import { expect, test } from "@playwright/test";
import { join } from "node:path";

import { findFirstFailingJob } from "./review-selection";

const outputDir = process.env.SCREENSHOT_DIR ?? "/output";

test("review workspace with an open finding", async ({ page }) => {
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
