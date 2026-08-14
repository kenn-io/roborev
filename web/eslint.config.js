import eslint from "@eslint/js";
import svelte from "eslint-plugin-svelte";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  {
    ignores: ["dist/**", "coverage/**", "src/lib/api/generated.ts"],
  },
  eslint.configs.recommended,
  ...tseslint.configs.recommended,
  ...svelte.configs["flat/recommended"],
  {
    files: ["src/**/*.svelte", "src/**/*.svelte.js", "src/**/*.svelte.ts"],
    languageOptions: {
      parserOptions: {
        parser: tseslint.parser,
      },
    },
  },
  {
    files: ["src/**/*.{ts,svelte}"],
    languageOptions: {
      globals: globals.browser,
    },
  },
  {
    files: ["src/**/*.svelte.js", "src/**/*.svelte.ts"],
    rules: {
      "svelte/prefer-svelte-reactivity": "off",
    },
  },
  {
    files: ["*.config.{js,ts}", "scripts/**/*.ts"],
    languageOptions: {
      globals: globals.node,
    },
  },
);
