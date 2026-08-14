import { Clipboard } from "@effect/platform-browser";
import { Button, EmptyState, Modal } from "@kenn-io/kit-ui";
import CircleCheckIcon from "@lucide/svelte/icons/circle-check";
import DOMPurify from "dompurify";
import { Effect } from "effect";
import { Marked } from "marked";
import mermaid from "mermaid";
import { getSingletonHighlighter } from "shiki";
import { expect, test } from "vitest";

test("review transplant dependencies expose their browser contracts", () => {
  expect(Clipboard).toBeDefined();
  expect(Button).toBeDefined();
  expect(EmptyState).toBeDefined();
  expect(Modal).toBeDefined();
  expect(CircleCheckIcon).toBeDefined();
  expect(DOMPurify).toBeDefined();
  expect(Effect).toBeDefined();
  expect(Marked).toBeDefined();
  expect(mermaid).toBeDefined();
  expect(getSingletonHighlighter).toBeTypeOf("function");
});
