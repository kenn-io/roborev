import { expect, type Locator, type Page } from "@playwright/test";

export async function findFirstFailingJob(page: Page): Promise<Locator> {
  const jobs = page.getByRole("region", { name: "Review jobs" });
  const jobRows = jobs.locator(".job-row");
  const failingJob = jobs.locator(".job-row", {
    has: page.locator(".verdict-fail"),
  });
  const olderJobs = jobs.locator(".scroll-sentinel");

  await expect(jobRows.first()).toBeVisible();
  while ((await failingJob.count()) === 0) {
    if ((await olderJobs.count()) === 0) {
      throw new Error("No failing review is available for the screenshot");
    }
    const loadedCount = await jobRows.count();
    await jobs.evaluate((element) => {
      element.scrollTop = element.scrollHeight;
    });
    await expect.poll(() => jobRows.count()).toBeGreaterThan(loadedCount);
  }

  await expect(failingJob.first()).toBeVisible();
  return failingJob.first();
}
