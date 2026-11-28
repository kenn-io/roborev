import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

import { expect, test, type Locator } from "@playwright/test";

import { openReview, openReviews } from "./support";

// Headless Chromium hides scrollbars by default, which would mask a
// regression back to classic layout-consuming scrollbars.
test.use({
  launchOptions: { ignoreDefaultArgs: ["--hide-scrollbars"] },
});

function overflows(element: Locator): Promise<boolean> {
  return element.evaluate((node) => node.scrollHeight > node.clientHeight);
}

function scrollbarWidth(element: Locator): Promise<number> {
  return element.evaluate(
    (node) => (node as HTMLElement).offsetWidth - node.clientWidth,
  );
}

async function left(element: Locator): Promise<number | undefined> {
  return (await element.boundingBox())?.x;
}

test("keeps the centered release notes column in place when a longer release scrolls", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 700 });
  await page.route("**/api/releases", async (route) => {
    await route.fulfill({
      json: {
        releases: [
          {
            tag_name: "v2.0.0",
            name: "Short release",
            body: "## Summary\n\nOne fix.",
            html_url: "https://example.test/releases/v2.0.0",
            published_at: "2026-09-01T12:00:00Z",
            prerelease: false,
          },
          {
            tag_name: "v1.0.0",
            name: "Long release",
            body: `## Summary\n\n${"A synthetic paragraph.\n\n".repeat(60)}`,
            html_url: "https://example.test/releases/v1.0.0",
            published_at: "2026-08-01T12:00:00Z",
            prerelease: false,
          },
        ],
        fetched_at: "2026-09-01T12:00:00Z",
        stale: false,
      },
    });
  });
  await openReviews(page);
  await page.getByRole("button", { name: "Release notes" }).click();

  const copyFrame = page.locator(".release-copy-frame");
  const copy = copyFrame.locator(".release-copy");
  const heading = copy.getByRole("heading", { name: "Summary" });
  await expect(heading).toBeVisible();
  expect(await overflows(copy)).toBe(false);
  const shortX = await left(heading);

  await page.getByRole("button", { name: /Long release/ }).click();
  await expect.poll(() => overflows(copy)).toBe(true);
  expect(await scrollbarWidth(copy)).toBe(0);
  await copy.hover();
  await expect(
    copyFrame.locator('.kit-scrollbar--y[data-visible="true"]'),
  ).toBeVisible();

  expect(await left(heading)).toBe(shortX);
});

test("keeps repository counts in place when expanding branches makes the picker scroll", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1700, height: 900 });
  await page.route(/\/api\/repos(\?|$)/, async (route) => {
    const response = await route.fetch();
    const json = (await response.json()) as { repos: object[] };
    json.repos = Array.from({ length: 6 }, (_, index) => ({
      ...json.repos[0],
      name: `repo-${index}`,
      root_path: `/synthetic/repo-${index}`,
      count: 3,
    }));
    await route.fulfill({ response, json });
  });
  await page.route(/\/api\/branches(\?|$)/, async (route) => {
    await route.fulfill({
      json: {
        branches: Array.from({ length: 12 }, (_, index) => ({
          name: `branch-${index}`,
          count: 1,
        })),
        total_count: 12,
        nulls_remaining: 0,
      },
    });
  });
  await openReviews(page);
  await page.locator(".picker-button").click();

  const dropdown = page.locator(".dropdown");
  const list = dropdown.locator(".dropdown-list");
  const row = page.locator(".repo-row", { hasText: "repo-2" });
  const count = row.locator(".count");
  await expect(count).toBeVisible();
  expect(await overflows(list)).toBe(false);
  const collapsedX = await left(count);

  await row.locator(".expand-btn").click();
  await expect(page.locator(".branch-item").first()).toBeVisible();
  expect(await overflows(list)).toBe(true);
  expect(await scrollbarWidth(list)).toBe(0);
  await expect(
    dropdown.locator('.kit-scrollbar--y[data-visible="true"]'),
  ).toBeVisible();

  expect(await left(count)).toBe(collapsedX);
});

test("keeps job table columns in place when opening a review makes the table scroll", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1700, height: 700 });
  await page.route(/\/api\/jobs(\?|$)/, async (route) => {
    const response = await route.fetch();
    const json = (await response.json()) as {
      jobs: object[];
      has_more: boolean;
    };
    json.jobs = json.jobs.slice(0, 14);
    json.has_more = false;
    await route.fulfill({ response, json });
  });
  await openReviews(page);

  const frame = page.locator(".table-frame");
  const table = frame.locator(".table-wrapper");
  const headerPositions = () =>
    page
      .locator(".job-table th")
      .evaluateAll((headers) =>
        headers.map((header) => header.getBoundingClientRect().x),
      );
  expect(await overflows(table)).toBe(false);
  const closedPositions = await headerPositions();

  await page.locator(".job-row").first().click();
  await expect(
    page.getByRole("region", { name: "Review details" }),
  ).toBeVisible();
  await expect.poll(() => overflows(table)).toBe(true);
  expect(await scrollbarWidth(table)).toBe(0);
  await table.hover();
  await expect(
    frame.locator('.kit-scrollbar--y[data-visible="true"]'),
  ).toBeVisible();

  expect(await headerPositions()).toEqual(closedPositions);
});

test("keeps a rich review expanded inside the dock without triggering a root scrollbar", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await openReview(page, 52);
  await expect(
    page.locator(".review-content .kit-mermaid-viewer__pan svg.flowchart"),
  ).toBeVisible();

  expect(
    await page.evaluate(
      () => window.innerWidth - document.documentElement.clientWidth,
    ),
  ).toBe(0);

  const reviewContent = page.locator(".review-content");
  expect(await overflows(reviewContent)).toBe(false);

  const dockFrame = page.locator(".kit-bottom-dock__body-frame");
  const dockBody = dockFrame.locator(".kit-bottom-dock__body");
  await expect.poll(() => overflows(dockBody)).toBe(true);
  expect(await scrollbarWidth(dockBody)).toBe(0);
  await dockBody.hover();
  await expect(
    dockFrame.locator('.kit-scrollbar--y[data-visible="true"]'),
  ).toBeVisible();
});

test("keeps the login card in place when it makes a short window scroll, then returns the width", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1700, height: 360 });
  let releaseSession!: () => void;
  const sessionReleased = new Promise<void>((resolveRelease) => {
    releaseSession = resolveRelease;
  });
  await page.route("**/api/**", async (route) => {
    await sessionReleased;
    await route.continue();
  });
  await page.goto("/reviews");
  await expect(page.getByText("Checking browser session")).toBeVisible();

  const appFrame = page.locator(".app-frame");
  const scroller = appFrame.locator("main");
  const brand = page.locator(".brand-lockup");
  expect(await overflows(scroller)).toBe(false);
  const checkingX = await left(brand);

  releaseSession();
  const login = page.getByLabel("Daemon token");
  await expect(login).toBeVisible();
  expect(await overflows(scroller)).toBe(true);
  expect(await scrollbarWidth(scroller)).toBe(0);
  await scroller.hover();
  await expect(
    appFrame.locator('.kit-scrollbar--y[data-visible="true"]'),
  ).toBeVisible();
  expect(await left(brand)).toBe(checkingX);

  await login.fill(process.env.ROBOREV_E2E_TOKEN ?? "");
  await page.getByRole("button", { name: "Connect" }).click();
  const application = page.locator("main.application");
  await expect(application).toBeVisible();
  expect((await application.boundingBox())?.width).toBe(1700);
});

test("keeps the public site header in place between short and long pages", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1700, height: 1000 });
  const siteOrigin = "http://roborev-site.test";
  const siteFiles: Record<string, [file: string, contentType: string]> = {
    "/": ["index.html", "text/html"],
    "/404.html": ["404.html", "text/html"],
    "/styles/site.css": ["styles/site.css", "text/css"],
  };
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    const entry = url.origin === siteOrigin ? siteFiles[url.pathname] : null;
    if (!entry) {
      await route.abort();
      return;
    }
    await route.fulfill({
      contentType: entry[1],
      body: await readFile(resolve("../docs/website", entry[0]), "utf8"),
    });
  });

  const root = page.locator("html");
  const wordmark = page.locator(".wordmark");
  await page.goto(`${siteOrigin}/404.html`);
  expect(await overflows(root)).toBe(false);
  const shortPageX = await left(wordmark);

  await page.goto(`${siteOrigin}/`);
  expect(await overflows(root)).toBe(true);
  expect(await left(wordmark)).toBe(shortPageX);
});
