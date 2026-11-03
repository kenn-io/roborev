import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // In CI, retry failed tests and write a JSON report so a flaky test is
    // recorded as passed with the failure messages of its earlier attempts.
    retry: process.env.CI ? 2 : 0,
    reporters: process.env.CI
      ? [
          "default",
          "github-actions",
          ["json", { outputFile: "test-results/vitest/roborev-ui.json" }],
        ]
      : ["default"],
  },
});
