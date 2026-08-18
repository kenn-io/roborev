import { afterEach, describe, expect, test } from "vitest";

import { appPath, getBasePath, stripBasePath } from "./base-path";

afterEach(() => {
  document.head.querySelector('meta[name="roborev-base-path"]')?.remove();
  history.replaceState(null, "", "/");
});

function setBasePath(value: string): void {
  const meta = document.createElement("meta");
  meta.name = "roborev-base-path";
  meta.content = value;
  document.head.append(meta);
}

describe("browser base path", () => {
  test("defaults to the origin root", () => {
    expect(getBasePath()).toBe("");
    expect(appPath("/api/status")).toBe("/api/status");
    expect(stripBasePath("/reviews")).toBe("/reviews");
  });

  test("matches the exact configured prefix", () => {
    setBasePath("/roborev-ci");

    expect(getBasePath()).toBe("/roborev-ci");
    expect(appPath("/api/status")).toBe("/roborev-ci/api/status");
    expect(appPath("/")).toBe("/roborev-ci/");
    expect(stripBasePath("/roborev-ci/analytics")).toBe("/analytics");
    expect(stripBasePath("/roborev-ci")).toBe("/");
  });

  test("prefixes an internal route that overlaps the configured prefix", () => {
    setBasePath("/reviews");

    expect(appPath("/reviews/42")).toBe("/reviews/reviews/42");
  });

  test("does not treat a near match as an application path", () => {
    setBasePath("/roborev-ci");

    expect(stripBasePath("/roborev-cinema")).toBe("");
  });
});
