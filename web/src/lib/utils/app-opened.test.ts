import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

type AppOpenedModule = typeof import("./app-opened");

function accepted(): Response {
  return new Response(JSON.stringify({ status: "queued" }), {
    status: 202,
    headers: { "Content-Type": "application/json" },
  });
}

function settle(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

function focusWindow(): void {
  globalThis.dispatchEvent(new Event("focus"));
}

describe("setupAppOpenedReporting", () => {
  let appOpened: AppOpenedModule;
  let fetchMock: ReturnType<
    typeof vi.fn<(input: Request) => Promise<Response>>
  >;
  const cleanups: Array<() => void> = [];

  function setup(): () => void {
    const cleanup = appOpened.setupAppOpenedReporting();
    cleanups.push(cleanup);
    return cleanup;
  }

  beforeEach(async () => {
    vi.resetModules();
    appOpened = await import("./app-opened");
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-03-10T09:00:00Z"));
    sessionStorage.clear();
    sessionStorage.setItem("roborev.web.session", "tab-session");
    sessionStorage.setItem("roborev.web.csrf", "csrf-value");
    document.head.querySelector('meta[name="roborev-base-path"]')?.remove();
    fetchMock = vi.fn(async () => accepted());
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    for (const cleanup of cleanups.splice(0)) cleanup();
    document.head.querySelector('meta[name="roborev-base-path"]')?.remove();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  test("posts one app_opened event with session and CSRF headers on load", async () => {
    setup();
    await settle();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const request = fetchMock.mock.calls[0]![0];
    expect(new URL(request.url).pathname).toBe("/api/telemetry/events");
    expect(request.method).toBe("POST");
    expect(await request.text()).toBe(
      '{"event":"app_opened","properties":{"surface":"web"}}',
    );
    expect(request.headers.get("X-Roborev-Web-Session")).toBe("tab-session");
    expect(request.headers.get("X-Roborev-CSRF")).toBe("csrf-value");
    expect(request.headers.get("Content-Type")).toBe("application/json");
  });

  test("ignores focus later on the same UTC day", async () => {
    setup();
    focusWindow();
    vi.setSystemTime(new Date("2026-03-10T20:00:00Z"));
    focusWindow();
    await settle();

    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test("posts again on the first focus of the next UTC day only", async () => {
    setup();
    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    focusWindow();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    vi.setSystemTime(new Date("2026-03-11T15:00:00Z"));
    focusWindow();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test("uses the UTC day boundary", async () => {
    vi.setSystemTime(new Date("2026-03-10T23:59:00Z"));
    setup();
    vi.setSystemTime(new Date("2026-03-11T00:01:00Z"));
    focusWindow();
    await settle();

    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test("sends nothing when the shell remounts the same day", async () => {
    setup()();
    setup();
    await settle();

    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test("sends nothing when the shell remounts on a later day without focus", async () => {
    setup()();
    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    setup();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    focusWindow();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test("resolves the configured base path", async () => {
    const meta = document.createElement("meta");
    meta.name = "roborev-base-path";
    meta.content = "/roborev";
    document.head.append(meta);

    setup();
    await settle();

    const request = fetchMock.mock.calls[0]![0];
    expect(new URL(request.url).pathname).toBe("/roborev/api/telemetry/events");
  });

  test.each([
    ["rejected", () => Promise.reject(new TypeError("network down"))],
    [
      "answered 400",
      async () => new Response("unsupported telemetry event", { status: 400 }),
    ],
  ])(
    "swallows a failed post (%s) and still reports the next day",
    async (_, fail) => {
      const unhandled = vi.fn();
      process.on("unhandledRejection", unhandled);
      try {
        fetchMock.mockImplementationOnce(fail);
        setup();
        await settle();
        await settle();
        expect(unhandled).not.toHaveBeenCalled();

        vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
        focusWindow();
        await settle();
        expect(fetchMock).toHaveBeenCalledTimes(2);
      } finally {
        process.off("unhandledRejection", unhandled);
      }
    },
  );

  test("holds a focus seen while the shell is gone until it returns", async () => {
    setup()();
    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    focusWindow();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    setup();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
