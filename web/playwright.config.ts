import { defineConfig, devices } from "@playwright/test";

const baseURL = process.env.ROBOREV_E2E_ORIGIN;
if (!baseURL) {
  throw new Error("run browser tests through `bun run test:e2e`");
}
// scripts/e2e.ts runs Playwright more than once; each run needs its own report.
const jsonReport = `test-results/playwright/${process.env.ROBOREV_E2E_REPORT ?? "web"}.json`;

export default defineConfig({
  testDir: "./tests/e2e",
  // Playwright empties outputDir when a run starts. Keep it apart from the
  // JSON reports so a later run does not delete an earlier run's report.
  outputDir: "test-results/artifacts",
  fullyParallel: false,
  workers: 1,
  timeout: 30_000,
  retries: process.env.CI ? 2 : 0,
  expect: { timeout: 8_000 },
  reporter: process.env.CI
    ? [
        ["line"],
        ["html", { open: "never" }],
        ["json", { outputFile: jsonReport }],
      ]
    : "line",
  use: {
    baseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
