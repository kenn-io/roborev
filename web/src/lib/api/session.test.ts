import { beforeEach, describe, expect, test, vi } from "vitest";

import {
  authenticatedFetch,
  bootstrapSession,
  clearTabSession,
  login,
  logout,
  sessionHeaders,
} from "./session";

const credentials = {
  session: "tab-session",
  csrf: "csrf-value",
  expires_at: "2026-08-13T20:00:00Z",
  capabilities: {
    cancel_any_job: false,
    cancel_review_job: true,
    rerun_job: false,
  },
};

function response(status: number, body?: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("browser session client", () => {
  beforeEach(() => {
    sessionStorage.clear();
    window.localStorage.clear();
    vi.restoreAllMocks();
  });

  test("bootstraps through the ambient cookie without a tab header", async () => {
    const fetchMock = vi.fn(async () => response(200, credentials));

    await expect(bootstrapSession(fetchMock)).resolves.toEqual({
      state: "authenticated",
      expiresAt: credentials.expires_at,
      capabilities: {
        cancelAnyJob: false,
        cancelReviewJob: true,
        rerunJob: false,
      },
    });
    expect(fetchMock).toHaveBeenCalledWith("/api/ui/session/bootstrap", {
      method: "POST",
      credentials: "same-origin",
      redirect: "error",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    expect(sessionStorage.getItem("roborev.web.session")).toBe(
      credentials.session,
    );
    expect(sessionStorage.getItem("roborev.web.csrf")).toBe(credentials.csrf);
    expect(window.localStorage.length).toBe(0);
  });

  test("exchanges the login token only in the JSON request body", async () => {
    const fetchMock = vi.fn(async () => response(200, credentials));

    await expect(login("one-time-secret", fetchMock)).resolves.toEqual({
      state: "authenticated",
      expiresAt: credentials.expires_at,
      capabilities: {
        cancelAnyJob: false,
        cancelReviewJob: true,
        rerunJob: false,
      },
    });
    const [url, options] = fetchMock.mock.calls[0] as unknown as [
      string,
      RequestInit,
    ];
    expect(url).toBe("/api/ui/session/login");
    expect(options.body).toBe(JSON.stringify({ token: "one-time-secret" }));
    expect(JSON.stringify({ url, options })).not.toContain("one-time-secret?");
    expect(window.localStorage.length).toBe(0);
  });

  test("returns the server login cooldown", async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ code: "login_rate_limited" }), {
          status: 429,
          headers: { "Content-Type": "application/json", "Retry-After": "8" },
        }),
    );

    await expect(login("one-time-secret", fetchMock)).resolves.toEqual({
      state: "rate-limited",
      retryAfterSeconds: 8,
    });
  });

  test("builds authenticated request headers from tab storage", async () => {
    const fetchMock = vi.fn(async () => response(200, credentials));
    await bootstrapSession(fetchMock);

    expect(sessionHeaders()).toEqual({
      "X-Roborev-Web-Session": credentials.session,
      "X-Roborev-CSRF": credentials.csrf,
    });
  });

  test("clears stale tab credentials when bootstrap requires login", async () => {
    sessionStorage.setItem("roborev.web.session", "stale");
    sessionStorage.setItem("roborev.web.csrf", "stale");

    await expect(
      bootstrapSession(vi.fn(async () => response(401))),
    ).resolves.toEqual({ state: "login-required" });
    expect(sessionStorage.length).toBe(0);
  });

  test("logout always clears tab credentials", async () => {
    sessionStorage.setItem("roborev.web.session", credentials.session);
    sessionStorage.setItem("roborev.web.csrf", credentials.csrf);
    const fetchMock = vi.fn(async () => {
      throw new Error("offline");
    });

    await expect(logout(fetchMock)).rejects.toThrow("offline");
    expect(fetchMock).toHaveBeenCalledWith("/api/ui/session", {
      method: "DELETE",
      credentials: "same-origin",
      redirect: "error",
      headers: {
        "X-Roborev-Web-Session": credentials.session,
        "X-Roborev-CSRF": credentials.csrf,
      },
    });
    expect(sessionStorage.length).toBe(0);
  });

  test("can clear only the tab-scoped credentials", () => {
    sessionStorage.setItem("roborev.web.session", "tab");
    sessionStorage.setItem("roborev.web.csrf", "csrf");
    window.localStorage.setItem("unrelated", "kept");

    clearTabSession();

    expect(sessionStorage.length).toBe(0);
    expect(window.localStorage.getItem("unrelated")).toBe("kept");
  });

  test("adds tab credentials to reads and CSRF credentials to mutations", async () => {
    sessionStorage.setItem("roborev.web.session", "tab");
    sessionStorage.setItem("roborev.web.csrf", "csrf");
    const inner = vi.fn(async (input: RequestInfo | URL) => {
      void input;
      return response(204);
    });
    const request = authenticatedFetch(inner);

    await request("/api/status", { headers: { Accept: "application/json" } });
    await request("/api/review/close", {
      method: "POST",
      body: JSON.stringify({ job_id: 7, closed: true }),
      signal: AbortSignal.timeout(1_000),
    });

    const read = inner.mock.calls[0]![0] as unknown as Request;
    const mutation = inner.mock.calls[1]![0] as unknown as Request;
    expect(read.credentials).toBe("same-origin");
    expect(read.headers.get("Accept")).toBe("application/json");
    expect(read.headers.get("X-Roborev-Web-Session")).toBe("tab");
    expect(read.headers.has("X-Roborev-CSRF")).toBe(false);
    expect(mutation.credentials).toBe("same-origin");
    expect(mutation.headers.get("X-Roborev-Web-Session")).toBe("tab");
    expect(mutation.headers.get("X-Roborev-CSRF")).toBe("csrf");
    expect(mutation.headers.get("Content-Type")).toBe("application/json");
    expect(mutation.signal.aborted).toBe(false);
  });

  test("clears rejected tab credentials after an authenticated request", async () => {
    sessionStorage.setItem("roborev.web.session", "stale");
    sessionStorage.setItem("roborev.web.csrf", "stale");
    const request = authenticatedFetch(vi.fn(async () => response(401)));

    await request("/api/status");

    expect(sessionStorage.length).toBe(0);
  });

  test("does not clear a newer session after a stale request is rejected", async () => {
    sessionStorage.setItem("roborev.web.session", "old-session");
    sessionStorage.setItem("roborev.web.csrf", "old-csrf");
    let rejectRequest: ((response: Response) => void) | undefined;
    const request = authenticatedFetch(
      vi.fn(
        () =>
          new Promise<Response>((resolve) => {
            rejectRequest = resolve;
          }),
      ),
    );

    const pending = request("/api/status");
    sessionStorage.setItem("roborev.web.session", "new-session");
    sessionStorage.setItem("roborev.web.csrf", "new-csrf");
    rejectRequest?.(response(401));
    await pending;

    expect(sessionStorage.getItem("roborev.web.session")).toBe("new-session");
    expect(sessionStorage.getItem("roborev.web.csrf")).toBe("new-csrf");
  });
});
