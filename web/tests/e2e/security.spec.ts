import { expect, request as playwrightRequest, test } from "@playwright/test";

import { openReview } from "./support";

test.describe.serial("browser listener security", () => {
  test("serves the public shell while protecting daemon APIs", async ({
    baseURL,
  }) => {
    const anonymous = await playwrightRequest.newContext({ baseURL });
    try {
      const shell = await anonymous.get("/reviews/52", {
        headers: { Accept: "text/html" },
      });
      expect(shell.status()).toBe(200);
      expect(shell.headers()["content-security-policy"]).toContain(
        "script-src 'self' 'wasm-unsafe-eval'",
      );
      expect(shell.headers()["content-security-policy"]).toContain(
        "style-src-elem 'self' 'unsafe-inline'",
      );

      expect((await anonymous.get("/api/jobs")).status()).toBe(401);
      expect((await anonymous.post("/shutdown")).status()).toBe(404);
      expect((await anonymous.get("/openapi.json")).status()).toBe(404);
    } finally {
      await anonymous.dispose();
    }
  });

  test("bootstraps a fresh deep-link tab from the ambient login session", async ({
    context,
  }) => {
    const loginPage = await context.newPage();
    await openReview(loginPage, 52);

    const page = await context.newPage();
    await openReview(page, 52);

    const storage = await page.evaluate(() => ({
      session: sessionStorage.getItem("roborev.web.session"),
      csrf: sessionStorage.getItem("roborev.web.csrf"),
      localCount: localStorage.length,
    }));
    expect(storage.session).toBeTruthy();
    expect(storage.csrf).toBeTruthy();
    expect(storage.localCount).toBe(0);
    expect(Object.values(storage)).not.toContain(process.env.ROBOREV_E2E_TOKEN);

    const rawRead = await page.evaluate(async () =>
      fetch("/api/jobs").then((response) => response.status),
    );
    expect(rawRead).toBe(401);
  });

  test("rejects a mutation without the tab CSRF credential", async ({
    page,
  }) => {
    await openReview(page, 52);

    const status = await page.evaluate(async () => {
      const session = sessionStorage.getItem("roborev.web.session") ?? "";
      return fetch("/api/review/close", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Roborev-Web-Session": session,
        },
        body: JSON.stringify({ job_id: 52, closed: true }),
      }).then((response) => response.status);
    });
    expect(status).toBe(403);
  });

  test("renders rich review content without CSP violations", async ({
    page,
  }) => {
    await page.addInitScript(() => {
      const violations: string[] = [];
      window.addEventListener("securitypolicyviolation", (event) => {
        violations.push(`${event.violatedDirective}:${event.blockedURI}`);
      });
      Object.defineProperty(window, "__roborevCSPViolations", {
        value: violations,
      });
    });

    await openReview(page, 52);
    await expect(page.locator(".review-content pre.shiki")).toBeVisible();
    await expect(page.locator(".review-content pre.mermaid")).toBeVisible();
    expect(
      await page.evaluate(
        () =>
          (window as Window & { __roborevCSPViolations?: string[] })
            .__roborevCSPViolations ?? [],
      ),
    ).toEqual([]);
  });

  test("requires a new login after the daemon restarts", async ({ page }) => {
    await openReview(page, 52);
    const controlOrigin = process.env.ROBOREV_E2E_CONTROL_ORIGIN;
    if (!controlOrigin)
      throw new Error("browser test control origin is missing");

    const restart = await fetch(`${controlOrigin}/restart`, { method: "POST" });
    expect(restart.status).toBe(204);
    await page.reload();

    const login = page.getByLabel("Daemon token");
    await expect(login).toBeVisible();
    await expect(page.getByRole("region", { name: "Review jobs" })).toHaveCount(
      0,
    );
    const token = process.env.ROBOREV_E2E_TOKEN;
    if (!token) throw new Error("browser test token is missing");
    await login.fill(token);
    await page.getByRole("button", { name: "Connect" }).click();
    await expect(
      page.getByRole("region", { name: "Review jobs" }),
    ).toBeVisible();
  });
});
