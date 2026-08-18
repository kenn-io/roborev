import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vite";

const backend = process.env.ROBOREV_WEB_DEV_BACKEND;

export default defineConfig({
  plugins: [svelte()],
  base: "./",
  optimizeDeps: {
    exclude: ["@kenn-io/kit-ui"],
  },
  build: {
    manifest: true,
  },
  server: {
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    proxy: backend
      ? {
          "/api": {
            target: backend,
            changeOrigin: false,
          },
        }
      : undefined,
  },
});
