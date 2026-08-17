import { expect, type Locator, type Page } from "@playwright/test";

export async function findFirstFailingJob(page: Page): Promise<Locator> {
  const jobRows = page.locator(".job-row");
  const failingJob = page.locator(".job-row", {
    has: page.locator(".verdict-fail"),
  });
  const loadMore = page.getByRole("button", {
    name: "Load more",
    exact: true,
  });

  await expect(jobRows.first()).toBeVisible();
  while ((await failingJob.count()) === 0) {
    if (!(await loadMore.isVisible())) {
      throw new Error("No failing review is available for the screenshot");
    }
    const loadedCount = await jobRows.count();
    await loadMore.click();
    await expect.poll(() => jobRows.count()).toBeGreaterThan(loadedCount);
  }

  await expect(failingJob.first()).toBeVisible();
  return failingJob.first();
}
