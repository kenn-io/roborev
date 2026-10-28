import { expect, test } from "@playwright/test";

import { findFirstFailingJob } from "./review-selection";

test("scrolls to older pages until a failing job appears", async ({ page }) => {
  await page.setContent(`
    <section aria-label="Review jobs" style="height: 100px; overflow: auto">
      <div class="job-row" style="height: 200px">
        <span class="verdict-pass">Pass</span>
      </div>
      <div class="scroll-sentinel" style="height: 1px"></div>
    </section>
    <script>
      const jobs = document.querySelector("section");
      let loadedPages = 0;
      jobs.addEventListener("scroll", () => {
        const sentinel = jobs.querySelector(".scroll-sentinel");
        if (!sentinel || jobs.scrollTop === 0) return;
        loadedPages += 1;
        sentinel.insertAdjacentHTML(
          "beforebegin",
          loadedPages === 2
            ? '<div class="job-row" style="height: 200px"><span class="verdict-fail">Fail</span></div>'
            : '<div class="job-row" style="height: 200px"><span class="verdict-pass">Pass</span></div>',
        );
        if (loadedPages === 2) sentinel.remove();
      });
    </script>
  `);

  const failingJob = await findFirstFailingJob(page);

  await expect(failingJob).toContainText("Fail");
  await expect(page.locator(".job-row")).toHaveCount(3);
});
