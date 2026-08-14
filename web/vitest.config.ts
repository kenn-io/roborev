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
    exclude: ["tests/e2e/**", "node_modules/**"],
    environment: "jsdom",
    environmentOptions: {
      jsdom: { url: "http://127.0.0.1/" },
    },
    setupFiles: ["./src/test/setup.ts"],
  },
});
