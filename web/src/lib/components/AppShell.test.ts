import { act, fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, expect, test, vi } from "vitest";
import * as composition from "../stores/composition.svelte";
import type { ReviewStoreOptions } from "../stores/composition.svelte";
import AppShell from "./AppShell.svelte";

vi.mock("../views/ReviewsView.svelte", async () => ({
  default: (await import("./reviews/ReviewDrawerTestContent.svelte")).default,
}));
vi.mock("../views/AnalyticsView.svelte", async () => ({
  default: (await import("./reviews/ReviewDrawerTestContent.svelte")).default,
}));

afterEach(() => vi.restoreAllMocks());

test("reports an alert once while visible and defers hidden errors until the tab returns", async () => {
  const events: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const request = input as Request;
    if (request.url.endsWith("/api/telemetry/events")) {
      const body = await request.json();
      if (body.event === "error_shown") events.push(body);
    }
    return new Response("{}", {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  });
  let hidden = false;
  vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
  let options: ReviewStoreOptions | undefined;
  const create = composition.createReviewStores;
  vi.spyOn(composition, "createReviewStores").mockImplementation((opts) => {
    options = opts;
    return create(opts);
  });
  const shell = render(AppShell, {
    capabilities: { cancelAnyJob: true, cancelReviewJob: true, rerunJob: true },
  });
  await act(() => options?.onError?.("Failed to load review", "review_viewed"));
  expect(screen.getByRole("alert")).toHaveTextContent("Failed to load review");
  await vi.waitFor(() => expect(events).toHaveLength(1));
  await act(() => options?.onError?.("Failed to load review", "review_viewed"));
  expect(events).toHaveLength(1);
  await fireEvent.click(screen.getByRole("button", { name: "Dismiss error" }));
  await act(() => options?.onError?.("Failed to load review", "review_viewed"));
  await vi.waitFor(() => expect(events).toHaveLength(2));
  await act(() => options?.onErrorCleared?.());
  hidden = true;
  await fireEvent(document, new Event("visibilitychange"));
  await act(() =>
    options?.onError?.("Failed to close review", "review_closed"),
  );
  expect(events).toHaveLength(2);
  hidden = false;
  await fireEvent(document, new Event("visibilitychange"));
  await vi.waitFor(() => expect(events).toHaveLength(3));
  await fireEvent(document, new Event("visibilitychange"));
  for (const [index, kind] of [
    "review_viewed",
    "review_viewed",
    "review_closed",
  ].entries()) {
    expect(events[index]).toEqual({
      event: "error_shown",
      properties: { surface: "web", kind },
    });
  }
  shell.unmount();
});
