import { expect, test } from "@playwright/test";

import { jobRow, openReview, openReviews, selectStatus } from "./support";

test.describe.serial("native review workspace", () => {
  test("renders application navigation as compact product chrome", async ({
    page,
  }) => {
    await openReviews(page);

    await expect(
      page.getByRole("button", { name: "Reviews", exact: true }),
    ).toHaveAttribute("aria-current", "page");
    await expect(
      page.getByRole("button", { name: "Analytics", exact: true }),
    ).toBeVisible();

    const brandStyle = await page.locator(".brand").evaluate((element) => {
      const style = getComputedStyle(element);
      return {
        fontSize: Number.parseFloat(style.fontSize),
        textDecorationLine: style.textDecorationLine,
      };
    });
    expect(brandStyle.fontSize).toBeLessThanOrEqual(16);
    expect(brandStyle.textDecorationLine).toBe("none");
  });

  test("keeps an opened review inside the application viewport", async ({
    page,
  }) => {
    const viewport = { width: 1280, height: 720 };
    await page.setViewportSize(viewport);
    await openReviews(page);
    await jobRow(page, 52).click();

    const dock = page.getByRole("region", { name: "Review details" });
    await expect(dock).toBeVisible();
    const geometry = await dock.evaluate((element) => {
      const bounds = element.getBoundingClientRect();
      return {
        top: bounds.top,
        bottom: bounds.bottom,
      };
    });
    const table = await page.locator(".table-wrapper").evaluate((element) => ({
      clientHeight: element.clientHeight,
      scrollHeight: element.scrollHeight,
      overflowY: getComputedStyle(element).overflowY,
    }));
    expect(geometry.top).toBeGreaterThanOrEqual(0);
    expect(geometry.bottom).toBeLessThanOrEqual(viewport.height);
    expect(table.scrollHeight).toBeGreaterThan(table.clientHeight);
    expect(table.overflowY).toBe("auto");
  });

  test("loads the seeded daemon and paginates real jobs", async ({ page }) => {
    await openReviews(page);

    await expect(page.locator(".job-row")).toHaveCount(50);
    await expect(page.getByText("project-alpha").first()).toBeVisible();
    await expect(page.getByText("project-beta").first()).toBeVisible();
    await expect(page.locator(".load-more-btn")).toBeVisible();

    await page.locator(".load-more-btn").click();
    await expect(page.locator(".job-row")).toHaveCount(54);
  });

  test("opens a review without downloading the full job history", async ({
    page,
  }) => {
    const unboundedJobRequests: string[] = [];
    page.on("request", (request) => {
      const url = new URL(request.url());
      if (
        url.pathname === "/api/jobs" &&
        url.searchParams.get("limit") === "0"
      ) {
        unboundedJobRequests.push(url.pathname);
      }
    });

    await openReviews(page);
    await jobRow(page, 52).click();

    await expect(
      page.getByRole("region", { name: "Review details" }),
    ).toBeVisible();
    await expect(page.locator(".review-dock-header .job-id")).toContainText(
      "52",
    );
    expect(unboundedJobRequests).toEqual([]);
  });

  test("filters and sorts the live listing", async ({ page }) => {
    await openReviews(page);
    await selectStatus(page, "Failed");

    await expect(
      page.locator(".status-badge.status-failed").first(),
    ).toBeVisible();
    await expect(page.locator(".status-badge:not(.status-failed)")).toHaveCount(
      0,
    );
    await expect(page.locator(".count-queued")).toContainText("0 queued");
    await expect(page.locator(".count-running")).toContainText("0 running");
    await expect(page.locator(".count-done")).toContainText("0 done");

    await selectStatus(page, "All statuses");
    const idHeader = page.locator("th", { hasText: "ID" });
    await expect(idHeader).toHaveAttribute("aria-disabled", "true");
    await page.locator(".load-more-btn").click();
    await expect(page.locator(".load-more-btn")).toBeHidden();
    await expect(idHeader).not.toHaveAttribute("aria-disabled", "true");
    const firstBefore = Number(
      (await page.locator(".col-id .mono").first().textContent())?.trim(),
    );
    await idHeader.click();
    await idHeader.click();
    const firstAfter = Number(
      (await page.locator(".col-id .mono").first().textContent())?.trim(),
    );
    expect(firstAfter).toBeLessThan(firstBefore);
  });

  test("filters by project, ref, and closed state", async ({ page }) => {
    await openReviews(page);

    await page.locator(".picker-button").click();
    await page.locator(".repo-item", { hasText: "project-beta" }).click();
    await expect(page.locator(".job-row").first()).toBeVisible();
    await expect(
      page.locator(".repo-name", { hasText: "project-alpha" }),
    ).toHaveCount(0);

    await page.locator(".picker-button").click();
    await page.locator(".dropdown-item", { hasText: /^All Repos$/ }).click();
    const search = page.getByRole("searchbox", { name: "Search by ref" });
    await search.fill("0000000000000000000000000000000000000034");
    await expect(page.locator(".job-row")).toHaveCount(1);
    await expect(jobRow(page, 52)).toBeVisible();

    await search.fill("");
    await expect(jobRow(page, 42)).toBeVisible();
    await page.getByRole("checkbox", { name: "Hide closed" }).check();
    await expect(jobRow(page, 42)).toHaveCount(0);
  });

  test("restores review preferences after a reload", async ({ page }) => {
    await openReviews(page);
    const hideClosed = page.getByRole("checkbox", { name: "Hide closed" });
    await hideClosed.check();
    await expect(jobRow(page, 42)).toHaveCount(0);

    await page.reload();
    await expect(hideClosed).toBeChecked();
    await expect(jobRow(page, 42)).toHaveCount(0);
  });

  test("shows an empty result without losing the filter controls", async ({
    page,
  }) => {
    await openReviews(page);
    await page
      .getByRole("searchbox", { name: "Search by ref" })
      .fill("no-such-fixture-ref");

    await expect(
      page.getByText("No jobs found", { exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("searchbox", { name: "Search by ref" }),
    ).toBeVisible();
    await expect(page.locator(".count-queued")).toContainText("0 queued");
    await expect(page.locator(".count-running")).toContainText("0 running");
    await expect(page.locator(".count-done")).toContainText("0 done");
    await expect(page.locator(".count-failed")).toContainText("0 failed");
  });

  test("opens a deep-linked rich review with comments", async ({ page }) => {
    await openReview(page, 52);

    await expect(page.locator(".review-dock-header .job-id")).toContainText(
      "52",
    );
    await expect(
      page.getByRole("heading", { name: "Streaming errors are discarded" }),
    ).toBeVisible();
    await expect(page.locator(".review-content pre.shiki")).toBeVisible();
    await expect(
      page.locator(".review-content .kit-mermaid-viewer svg.flowchart"),
    ).toBeVisible();
    await expect(page.locator(".response-item")).toHaveCount(2);
  });

  test("renders compact output, persisted logs, and the review prompt", async ({
    page,
  }) => {
    await openReview(page, 53);
    await expect(
      page.getByText("No issues found after consolidated review."),
    ).toBeVisible();
    await expect(
      page.locator(".col-type", { hasText: "compact" }),
    ).toBeVisible();

    await openReview(page, 52);
    await page.getByRole("button", { name: "Log", exact: true }).click();
    await expect(page.getByText("fixture review started")).toBeVisible();
    await expect(page.getByText("streamed analysis complete")).toBeVisible();
    await page.getByRole("button", { name: "Prompt", exact: true }).click();
    await expect(page.locator(".prompt-text")).toHaveText(
      "Review the fixture change",
    );
  });

  test("preserves deep links through reload and browser history", async ({
    page,
  }) => {
    await openReviews(page);
    await jobRow(page, 52).click();
    await expect(page).toHaveURL(/\/reviews\/52$/);

    await page.reload();
    await expect(page.locator(".review-dock-header .job-id")).toContainText(
      "52",
    );
    await page.goBack();
    await expect(page).toHaveURL(/\/reviews$/);
    await expect(
      page.getByRole("region", { name: "Review details" }),
    ).not.toBeVisible();
    await page.goForward();
    await expect(page).toHaveURL(/\/reviews\/52$/);
    await expect(
      page.getByRole("region", { name: "Review details" }),
    ).toBeVisible();
  });

  test("authenticates the fetch-based event stream", async ({ page }) => {
    const streamRequest = page.waitForRequest((request) =>
      request.url().includes("/api/stream/events"),
    );
    await openReviews(page);

    const request = await streamRequest;
    expect(request.headers()["x-roborev-web-session"]).toBeTruthy();
  });

  test("reconnects the event stream after a transport failure", async ({
    page,
  }) => {
    let attempts = 0;
    await page.route("**/api/stream/events", async (route) => {
      attempts += 1;
      if (attempts === 1) {
        await route.abort("connectionfailed");
        return;
      }
      await route.continue();
    });

    await openReviews(page);
    await expect.poll(() => attempts).toBeGreaterThanOrEqual(2);
  });

  test("recovers the workspace when daemon health returns", async ({
    page,
  }) => {
    await openReviews(page);
    let unavailable = true;
    await page.route("**/api/status", async (route) => {
      if (unavailable) {
        await route.fulfill({
          status: 503,
          contentType: "application/json",
          body: JSON.stringify({ error: "temporarily unavailable" }),
        });
        return;
      }
      await route.continue();
    });

    await page.reload();
    await expect(
      page.getByText("Roborev daemon not reachable", { exact: true }),
    ).toBeVisible();
    unavailable = false;
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(
      page.getByRole("region", { name: "Review jobs" }),
    ).toBeVisible();
    await expect(page.locator(".job-row").first()).toBeVisible();
  });

  test("keeps the prior listing visible when a filtered refresh fails", async ({
    page,
  }) => {
    await openReviews(page);
    await expect(jobRow(page, 52)).toBeVisible();
    await page.route("**/api/jobs?**", (route) =>
      route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ error: "temporarily unavailable" }),
      }),
    );

    await selectStatus(page, "Failed");
    await expect(page.locator(".error-bar")).toContainText(
      "Failed to load jobs",
    );
    await expect(jobRow(page, 52)).toBeVisible();

    await page.unroute("**/api/jobs?**");
    await selectStatus(page, "All statuses");
    await expect(page.locator(".error-bar")).toHaveCount(0);
    await expect(jobRow(page, 52)).toBeVisible();
  });

  test("retries a failed fetch-based job output stream", async ({ page }) => {
    let attempts = 0;
    await page.route("**/api/job/output?**", async (route) => {
      attempts += 1;
      if (attempts === 1) {
        await route.abort("connectionfailed");
        return;
      }
      await route.continue();
    });

    await openReview(page, 50);
    await page.getByRole("button", { name: "Log", exact: true }).click();
    await expect.poll(() => attempts).toBeGreaterThanOrEqual(2);
  });

  test("keeps the review table horizontally reachable in a narrow viewport", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 640, height: 720 });
    await openReviews(page);

    const overflow = await page
      .locator(".table-wrapper")
      .evaluate((element) => ({
        clientWidth: element.clientWidth,
        scrollWidth: element.scrollWidth,
        overflowX: getComputedStyle(element).overflowX,
      }));
    expect(overflow.scrollWidth).toBeGreaterThan(overflow.clientWidth);
    expect(overflow.overflowX).toBe("auto");
  });

  test("posts a comment through the daemon mutation contract", async ({
    page,
  }) => {
    await openReview(page, 52);
    const comment = "Browser parity comment";

    await page.locator(".comment-textarea").fill(comment);
    await page.locator(".submit-btn").click();

    await expect(
      page.locator(".response-item", { hasText: comment }),
    ).toHaveCount(1);
    const response = await page.evaluate(async () => {
      const session = sessionStorage.getItem("roborev.web.session");
      const result = await fetch("/api/comments?job_id=52", {
        headers: { "X-Roborev-Web-Session": session ?? "" },
      });
      return { status: result.status, body: await result.text() };
    });
    expect(response.status).toBe(200);
    expect(response.body).toContain(comment);
  });

  test("closes a review and reflects the authoritative result", async ({
    page,
  }) => {
    await openReview(page, 51);
    const actions = page.getByRole("group", { name: "Review actions" });

    await actions.getByRole("button", { name: "Close Review" }).click();
    await expect(actions.getByRole("button", { name: "Reopen" })).toBeVisible();
    await actions.getByRole("button", { name: "Reopen" }).click();
    await expect(
      actions.getByRole("button", { name: "Close Review" }),
    ).toBeVisible();
  });

  test("shows only actions supported by a token session", async ({ page }) => {
    await openReview(page, 50);
    const queuedActions = page.getByRole("group", { name: "Review actions" });
    await queuedActions.getByRole("button", { name: "Cancel" }).click();
    await expect(
      page.locator(".review-dock-header .status-badge"),
    ).toContainText("canceled");

    await openReview(page, 48);
    const failedActions = page.getByRole("group", { name: "Review actions" });
    await expect(
      failedActions.getByRole("button", { name: "Rerun" }),
    ).toHaveCount(0);
  });

  test(
    "reruns jobs through a local session",
    { tag: "@local-session" },
    async ({ page }) => {
      await openReview(page, 48);
      const actions = page.getByRole("group", { name: "Review actions" });
      await actions.getByRole("button", { name: "Rerun" }).click();
      await expect(
        page.locator(".review-dock-header .status-badge"),
      ).toContainText("queued");
    },
  );

  test("expands panel members fetched from the daemon", async ({ page }) => {
    await openReviews(page);
    const parent = jobRow(page, 56);
    await expect(parent).toBeVisible();

    await parent.getByRole("button", { name: "Expand panel" }).click();

    await expect(page.locator(".job-row.member")).toHaveCount(2);
    await expect(
      page.locator(".member-name", { hasText: "correctness" }),
    ).toBeVisible();
    await expect(
      page.locator(".member-name", { hasText: "security" }),
    ).toBeVisible();
  });

  test("preserves transplanted keyboard navigation and help", async ({
    page,
  }) => {
    await openReviews(page);

    await page.keyboard.press("?");
    await expect(
      page.getByText("Keyboard Shortcuts", { exact: true }),
    ).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(
      page.getByText("Keyboard Shortcuts", { exact: true }),
    ).toHaveCount(0);

    await page.keyboard.press("j");
    const highlighted = page.locator(".job-row.highlighted");
    await expect(highlighted).toHaveCount(1);
    const id = (
      await highlighted.locator(".col-id .mono").textContent()
    )?.trim();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/reviews/${id}$`));
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(/\/reviews$/);
  });
});
