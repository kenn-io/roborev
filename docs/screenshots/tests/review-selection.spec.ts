import { expect, test } from "@playwright/test";

import { findFirstFailingJob } from "./review-selection";

test("loads additional pages until a failing job appears", async ({ page }) => {
  await page.setContent(`
    <section aria-label="Review jobs">
      <div class="job-row"><span class="verdict-pass">Pass</span></div>
      <button type="button">Load more</button>
    </section>
    <script>
      let loadedPages = 0;
      document.querySelector("button").addEventListener("click", () => {
        loadedPages += 1;
        document.querySelector("section").insertAdjacentHTML(
          "afterbegin",
          loadedPages === 2
            ? '<div class="job-row"><span class="verdict-fail">Fail</span></div>'
            : '<div class="job-row"><span class="verdict-pass">Pass</span></div>',
        );
        if (loadedPages === 2) document.querySelector("button").remove();
      });
    </script>
  `);

  const failingJob = await findFirstFailingJob(page);

  await expect(failingJob).toContainText("Fail");
  await expect(page.locator(".job-row")).toHaveCount(3);
});
