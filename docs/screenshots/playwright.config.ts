import { defineConfig } from "@playwright/test";

const baseURL = process.env.ROBOREV_SCREENSHOT_ORIGIN;
if (!baseURL) {
  throw new Error("ROBOREV_SCREENSHOT_ORIGIN is required");
}

export default defineConfig({
  testDir: "./tests",
  timeout: 60_000,
  retries: 0,
  workers: 1,
  use: {
    baseURL,
    viewport: { width: 1440, height: 900 },
    colorScheme: "dark",
    timezoneId: "America/Chicago",
  },
  projects: [
    {
      name: "screenshots",
      use: { browserName: "chromium" },
    },
  ],
});
