import { fireEvent, render, screen } from "@testing-library/svelte";
import { beforeEach, describe, expect, test, vi } from "vitest";

import App from "./App.svelte";

const credentials = {
  session: "tab-session",
  csrf: "csrf-value",
  expires_at: "2026-08-13T20:00:00Z",
  capabilities: {
    cancel_any_job: true,
    cancel_review_job: true,
    rerun_job: true,
  },
};

function response(status: number, body?: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const status = {
  active_workers: 1,
  canceled_jobs: 0,
  completed_jobs: 1,
  failed_jobs: 0,
  max_workers: 2,
  queued_jobs: 0,
  running_jobs: 0,
  version: "test",
};

function applicationResponse(input: RequestInfo | URL): Response {
  const url = new URL(
    input instanceof Request ? input.url : input,
    location.origin,
  );
  if (url.pathname === "/api/status") return response(200, status);
  if (url.pathname === "/api/jobs") {
    return response(200, {
      jobs: [],
      has_more: false,
      stats: { done: 0, closed: 0, open: 0 },
    });
  }
  if (url.pathname === "/api/stream/events") {
    return new Response(new ReadableStream({ start() {} }), { status: 200 });
  }
  return response(404);
}

describe("App", () => {
  beforeEach(() => {
    sessionStorage.clear();
    document.documentElement.classList.remove("dark");
    document.head.querySelector('meta[name="roborev-base-path"]')?.remove();
    history.replaceState(null, "", "/reviews");
    vi.restoreAllMocks();
  });

  test("checks the ambient browser session on mount", () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>(() => {})),
    );
    render(App);

    expect(screen.getByText("Checking browser session…")).toBeInTheDocument();
    expect(document.documentElement).toHaveClass("dark");
  });

  test("renders token login when remote authentication is required", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => response(401)),
    );
    render(App);

    expect(
      await screen.findByRole("heading", { name: "Connect to Roborev" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Daemon token")).toHaveAttribute(
      "autocomplete",
      "current-password",
    );
  });

  test("clears the token input after login and renders the review workspace", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(response(401))
      .mockResolvedValueOnce(response(200, credentials))
      .mockImplementation(applicationResponse);
    vi.stubGlobal("fetch", fetchMock);
    render(App);

    const token = await screen.findByLabelText("Daemon token");
    await fireEvent.input(token, { target: { value: "one-time-secret" } });
    await fireEvent.click(screen.getByRole("button", { name: "Connect" }));

    expect(
      await screen.findByRole("region", { name: "Review jobs" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Roborev" })).toHaveAttribute(
      "href",
      "/reviews",
    );
    expect(screen.getByRole("button", { name: "Reviews" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    await fireEvent.click(screen.getByRole("button", { name: "Analytics" }));
    expect(
      screen.getByRole("heading", { name: "Project review health" }),
    ).toBeInTheDocument();
    expect(location.pathname).toBe("/analytics");
    expect(token).toHaveValue("");
    expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull();
    expect(JSON.stringify(fetchMock.mock.calls)).toContain("one-time-secret");
  });

  test("uses the configured prefix for the application shell and navigation", async () => {
    const meta = document.createElement("meta");
    meta.name = "roborev-base-path";
    meta.content = "/roborev-ci";
    document.head.append(meta);
    history.replaceState(null, "", "/roborev-ci/reviews");

    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(
        input instanceof Request ? input.url : input,
        location.origin,
      );
      if (url.pathname === "/roborev-ci/api/ui/session/bootstrap") {
        return response(200, credentials);
      }
      if (url.pathname.startsWith("/roborev-ci/")) {
        const internal = new URL(url);
        internal.pathname = url.pathname.slice("/roborev-ci".length) || "/";
        return applicationResponse(internal);
      }
      return response(404);
    });
    vi.stubGlobal("fetch", fetchMock);
    render(App);

    expect(
      await screen.findByRole("region", { name: "Review jobs" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Roborev" })).toHaveAttribute(
      "href",
      "/roborev-ci/reviews",
    );
    await fireEvent.click(screen.getByRole("button", { name: "Analytics" }));
    expect(location.pathname).toBe("/roborev-ci/analytics");
  });

  test("keeps login available and shows the server cooldown after throttling", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(response(401))
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ code: "login_rate_limited" }), {
          status: 429,
          headers: { "Content-Type": "application/json", "Retry-After": "60" },
        }),
      );
    vi.stubGlobal("fetch", fetchMock);
    render(App);

    const input = await screen.findByLabelText("Daemon token");
    await fireEvent.input(input, { target: { value: "wrong-token" } });
    await fireEvent.submit(input.closest("form")!);

    expect(
      await screen.findByText(/Too many attempts\. Try again in \d+ seconds\./),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Daemon token")).toBeDisabled();
    expect(
      screen.getByRole("button", { name: /Try again in \d+s/ }),
    ).toBeDisabled();
    expect(
      screen.queryByRole("heading", { name: "Roborev is unavailable" }),
    ).toBeNull();
  });

  test("preserves modified clicks on the product link", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = new URL(
          input instanceof Request ? input.url : input,
          location.origin,
        );
        if (url.pathname === "/api/ui/session/bootstrap") {
          return response(200, credentials);
        }
        return applicationResponse(input);
      }),
    );
    render(App);

    await fireEvent.click(
      await screen.findByRole("button", { name: "Analytics" }),
    );
    const brand = screen.getByRole("link", { name: "Roborev" });
    const click = new MouseEvent("click", {
      bubbles: true,
      cancelable: true,
      ctrlKey: true,
    });
    brand.dispatchEvent(click);

    expect(click.defaultPrevented).toBe(false);
    expect(location.pathname).toBe("/analytics");
  });

  test("offers a retry after a bootstrap error", async () => {
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new Error("offline"))
      .mockResolvedValueOnce(response(200, credentials))
      .mockImplementation(applicationResponse);
    vi.stubGlobal("fetch", fetchMock);
    render(App);

    expect(await screen.findByText("offline")).toBeInTheDocument();
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(
      await screen.findByRole("region", { name: "Review jobs" }),
    ).toBeInTheDocument();
  });

  test("does not expose session-management controls in the product shell", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(
        input instanceof Request ? input.url : input,
        location.origin,
      );
      if (url.pathname === "/api/ui/session/bootstrap") {
        return response(200, credentials);
      }
      return applicationResponse(input);
    });
    vi.stubGlobal("fetch", fetchMock);
    render(App);

    await screen.findByRole("region", { name: "Review jobs" });
    expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Disconnect" })).toBeNull();
  });

  test("shows action failures in the application shell", async () => {
    const job = {
      id: 17,
      agent: "test",
      agentic: false,
      enqueued_at: "2026-08-13T20:00:00Z",
      git_ref: "abc123",
      job_type: "review",
      prompt_prebuilt: false,
      repo_id: 1,
      repo_name: "example/repo",
      retry_count: 0,
      status: "queued",
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = new URL(
          input instanceof Request ? input.url : input,
          location.origin,
        );
        if (url.pathname === "/api/ui/session/bootstrap") {
          return response(200, credentials);
        }
        if (url.pathname === "/api/status") return response(200, status);
        if (url.pathname === "/api/jobs") {
          return response(200, {
            jobs: [job],
            has_more: false,
            stats: { queued: 1, done: 0, closed: 0, open: 1 },
          });
        }
        if (url.pathname === "/api/review") return response(404);
        if (url.pathname === "/api/ui/review-projection") {
          return response(200, { responses: [] });
        }
        if (url.pathname === "/api/job/cancel") {
          return response(500, { detail: "cancel unavailable" });
        }
        if (url.pathname === "/api/stream/events") {
          return new Response(new ReadableStream({ start() {} }), {
            status: 200,
          });
        }
        return response(404);
      }),
    );
    render(App);

    const row = await screen.findByRole("button", { name: /17.*abc123/i });
    await fireEvent.click(row);
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Failed to cancel job",
    );
  });

  test("refreshes an off-page deep-linked review from matching events", async () => {
    history.replaceState(null, "", "/reviews/42");
    const encoder = new TextEncoder();
    let eventStream: ReadableStreamDefaultController<Uint8Array> | undefined;
    let completed = false;
    let closed = false;
    const selectedJob = () => ({
      id: 42,
      agent: "test",
      agentic: false,
      enqueued_at: "2026-08-13T20:00:00Z",
      git_ref: "abc123",
      job_type: "review",
      prompt_prebuilt: false,
      repo_id: 1,
      repo_name: "example/repo",
      retry_count: 0,
      status: completed ? "done" : "running",
      ...(completed ? { finished_at: "2026-08-13T20:01:00Z" } : {}),
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = new URL(
          input instanceof Request ? input.url : input,
          location.origin,
        );
        if (url.pathname === "/api/ui/session/bootstrap") {
          return response(200, credentials);
        }
        if (url.pathname === "/api/status") return response(200, status);
        if (url.pathname === "/api/jobs") {
          if (url.searchParams.get("id") === "42") {
            return response(200, {
              jobs: [selectedJob()],
              has_more: false,
              stats: { done: completed ? 1 : 0, closed: 0, open: 1 },
            });
          }
          return response(200, {
            jobs: [],
            has_more: false,
            stats: { done: 0, closed: 0, open: 0 },
          });
        }
        if (url.pathname === "/api/review") {
          if (!completed) return response(404);
          return response(200, {
            id: 7,
            job_id: 42,
            output: "Fresh review output",
            prompt: "Review this change",
            closed,
            job: selectedJob(),
          });
        }
        if (url.pathname === "/api/ui/review-projection") {
          return response(200, { responses: [] });
        }
        if (url.pathname === "/api/stream/events") {
          return new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                eventStream = controller;
              },
            }),
            { status: 200 },
          );
        }
        return response(404);
      }),
    );
    render(App);

    expect(await screen.findByText("Review in progress…")).toBeInTheDocument();
    await vi.waitFor(() => expect(eventStream).toBeDefined());

    completed = true;
    eventStream?.enqueue(
      encoder.encode(
        `${JSON.stringify({
          type: "review.completed",
          ts: "2026-08-13T20:01:00Z",
          job_id: 42,
          repo: "/workspace/repo",
          repo_name: "repo",
          sha: "abc123",
        })}\n`,
      ),
    );
    expect(await screen.findByText("Fresh review output")).toBeInTheDocument();

    closed = true;
    eventStream?.enqueue(
      encoder.encode(
        `${JSON.stringify({
          type: "review.closed",
          ts: "2026-08-13T20:02:00Z",
          job_id: 42,
          repo: "/workspace/repo",
          repo_name: "repo",
          sha: "abc123",
        })}\n`,
      ),
    );
    expect(
      await screen.findByRole("button", { name: "Reopen" }),
    ).toBeInTheDocument();
  });

  test("returns to session bootstrap when the current session is rejected", async () => {
    let statusCalls = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(
        input instanceof Request ? input.url : input,
        location.origin,
      );
      if (url.pathname === "/api/ui/session/bootstrap") {
        return statusCalls === 0 ? response(200, credentials) : response(401);
      }
      if (url.pathname === "/api/status") {
        statusCalls += 1;
        return response(401);
      }
      return applicationResponse(input);
    });
    vi.stubGlobal("fetch", fetchMock);
    render(App);

    expect(
      await screen.findByRole("heading", { name: "Connect to Roborev" }),
    ).toBeInTheDocument();
  });
});
