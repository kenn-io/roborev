import { expect, type Locator, type Page } from "@playwright/test";

export function jobRow(page: Page, id: number) {
  return page.locator(".job-row").filter({
    has: page.locator(".col-id .mono", { hasText: new RegExp(`^${id}$`) }),
  });
}

export async function openReviews(
  page: Page,
  path = "/reviews",
): Promise<void> {
  await page.goto(path);
  const workspace = page.getByRole("region", { name: "Review jobs" });
  await authenticateIfNeeded(page, workspace);
  await expect(page.locator(".job-row").first()).toBeVisible();
}

export async function openAnalytics(
  page: Page,
  path = "/analytics?range=all",
): Promise<void> {
  await page.goto(path);
  const workspace = page.getByRole("heading", {
    name: "Project review health",
  });
  await authenticateIfNeeded(page, workspace);
  await expect(
    page.getByRole("table", { name: "Project analytics" }),
  ).toBeVisible();
}

async function authenticateIfNeeded(
  page: Page,
  workspace: Locator,
): Promise<void> {
  const login = page.getByLabel("Daemon token");
  await expect(workspace.or(login)).toBeVisible();
  if (await login.isVisible()) {
    const token = process.env.ROBOREV_E2E_TOKEN;
    if (!token) throw new Error("browser test token is not configured");
    await login.fill(token);
    await page.getByRole("button", { name: "Connect" }).click();
  }
  await expect(workspace).toBeVisible();
}

export async function openReview(page: Page, id: number): Promise<void> {
  await openReviews(page, `/reviews/${id}`);
  await expect(page).toHaveURL(new RegExp(`/reviews/${id}$`));
  await expect(
    page.getByRole("region", { name: "Review details" }),
  ).toBeVisible();
}

export async function selectStatus(page: Page, label: string): Promise<void> {
  await page
    .locator(".kit-filter-dropdown__btn", { hasText: "Status" })
    .click();
  await page.locator(".kit-filter-dropdown__item", { hasText: label }).click();
}
