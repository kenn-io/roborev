import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [svelte()],
  resolve: {
    conditions: ["browser"],
  },
  test: {
    // Svelte transforms are memory- and CPU-heavy enough that Vitest's
    // machine-sized default causes otherwise fast DOM tests to time out.
    maxWorkers: 4,
    // In CI, retry failed tests and write a JSON report so a flaky test is
    // recorded as passed with the failure messages of its earlier attempts.
    retry: process.env.CI ? 2 : 0,
    reporters: process.env.CI
      ? [
          "default",
          "github-actions",
          ["json", { outputFile: "test-results/vitest/web.json" }],
        ]
      : ["default"],
    exclude: ["tests/e2e/**", "node_modules/**"],
    environment: "jsdom",
    environmentOptions: {
      jsdom: { url: "http://127.0.0.1/" },
    },
    setupFiles: ["./src/test/setup.ts"],
  },
});
