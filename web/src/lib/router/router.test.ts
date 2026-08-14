import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { createRouter, parseRoute } from "./router.svelte";

describe("native router", () => {
  beforeEach(() => {
    history.replaceState(null, "", "/reviews");
  });

  afterEach(() => {
    history.replaceState(null, "", "/");
  });

  test("parses review deep links and rejects invalid job IDs", () => {
    expect(parseRoute("/reviews/42")).toEqual({ page: "reviews", jobId: 42 });
    expect(parseRoute("/reviews/not-a-job")).toEqual({ page: "reviews" });
    expect(parseRoute("/analytics")).toEqual({ page: "analytics" });
    expect(parseRoute("/unknown")).toEqual({ page: "reviews" });
  });

  test("pushes review selections and can replace the current route", () => {
    const router = createRouter();

    router.navigateToReview(17);
    expect(location.pathname).toBe("/reviews/17");
    expect(router.getRoute()).toEqual({ page: "reviews", jobId: 17 });

    router.navigateToReview(undefined, { replace: true });
    expect(location.pathname).toBe("/reviews");
    expect(router.getRoute()).toEqual({ page: "reviews" });
    router.dispose();
  });

  test("updates from browser history and routes to analytics", () => {
    const router = createRouter();

    history.pushState(null, "", "/reviews/9");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(router.getRoute()).toEqual({ page: "reviews", jobId: 9 });

    router.navigateToAnalytics();
    expect(router.getRoute()).toEqual({ page: "analytics" });
    expect(location.pathname).toBe("/analytics");
    router.dispose();
  });
});
