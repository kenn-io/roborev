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
  const listeners: Array<
    [EventTarget, string, EventListenerOrEventListenerObject]
  > = [];

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
    const addWindow = globalThis.addEventListener.bind(globalThis);
    vi.spyOn(globalThis, "addEventListener").mockImplementation(
      (type, listener, options) => {
        listeners.push([globalThis, type, listener]);
        addWindow(type, listener, options);
      },
    );
    const addDocument = document.addEventListener.bind(document);
    vi.spyOn(document, "addEventListener").mockImplementation(
      (type, listener, options) => {
        listeners.push([document, type, listener]);
        addDocument(type, listener, options);
      },
    );
  });

  afterEach(() => {
    for (const cleanup of cleanups.splice(0)) cleanup();
    for (const [target, type, listener] of listeners.splice(0))
      target.removeEventListener(type, listener);
    document.head.querySelector('meta[name="roborev-base-path"]')?.remove();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
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

  test.each([
    [59_999, "under_1m"],
    [60_000, "1_to_5m"],
    [120_000, "1_to_5m"],
    [300_000, "5_to_30m"],
    [1_800_000, "5_to_30m"],
    [1_800_001, "over_30m"],
  ])(
    "reports %i visible milliseconds as %s with closing credentials",
    async (elapsed, bucket) => {
      let now = 0;
      vi.spyOn(performance, "now").mockImplementation(() => now);
      vi.spyOn(document, "hidden", "get").mockReturnValue(false);
      setup();
      await settle();
      now = elapsed;
      globalThis.dispatchEvent(new Event("pagehide"));
      globalThis.dispatchEvent(new Event("pagehide"));
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(2);
      const request = fetchMock.mock.calls[1]![0];
      expect(JSON.parse(await request.text())).toEqual({
        event: "session_ended",
        properties: { surface: "web", duration_bucket: bucket },
      });
      expect(request.keepalive).toBe(true);
      expect(request.headers.get("X-Roborev-Web-Session")).toBe("tab-session");
      expect(request.headers.get("X-Roborev-CSRF")).toBe("csrf-value");
    },
  );

  test("starts on visible return and excludes hidden time and disposed mounts", async () => {
    let now = 0;
    let hidden = true;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
    const stop = setup();
    now = 600_000;
    globalThis.dispatchEvent(new Event("pagehide"));
    hidden = false;
    document.dispatchEvent(new Event("visibilitychange"));
    now += 120_000;
    hidden = true;
    document.dispatchEvent(new Event("visibilitychange"));
    globalThis.dispatchEvent(new Event("pagehide"));
    now += 600_000;
    hidden = false;
    globalThis.dispatchEvent(new Event("pageshow"));
    now += 120_000;
    globalThis.dispatchEvent(new Event("pagehide"));
    stop();
    globalThis.dispatchEvent(new Event("pageshow"));
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(3);
    for (const [request] of fetchMock.mock.calls.slice(1)) {
      expect(JSON.parse(await request.text()).properties.duration_bucket).toBe(
        "1_to_5m",
      );
    }
  });

  test("keeps visible time across shell recovery and excludes the recovery gap", async () => {
    let now = 0;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    const stop = setup();
    now = 120_000;
    stop();
    now += 600_000;
    setup();
    now += 10_000;
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    const request = fetchMock.mock.calls[1]![0];
    expect(JSON.parse(await request.text()).properties.duration_bucket).toBe(
      "1_to_5m",
    );
  });

  test.each([true, false])(
    "flushes a hide during recovery once, remount hidden=%s",
    async (remountHidden) => {
      let now = 0;
      let hidden = false;
      vi.spyOn(performance, "now").mockImplementation(() => now);
      vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
      const stop = setup();
      now = 120_000;
      stop();
      now += 10_000;
      hidden = true;
      document.dispatchEvent(new Event("visibilitychange"));
      globalThis.dispatchEvent(new Event("pagehide"));
      expect(fetchMock).toHaveBeenCalledTimes(1);
      now += 600_000;
      hidden = remountHidden;
      document.dispatchEvent(new Event("visibilitychange"));
      sessionStorage.setItem("roborev.web.session", "recovered-session");
      setup();
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(2);
      const request = fetchMock.mock.calls[1]![0];
      expect(request.headers.get("X-Roborev-Web-Session")).toBe(
        "recovered-session",
      );
      expect(JSON.parse(await request.text()).properties.duration_bucket).toBe(
        "1_to_5m",
      );
      if (remountHidden) globalThis.dispatchEvent(new Event("pagehide"));
      else {
        now += 10_000;
        globalThis.dispatchEvent(new Event("pagehide"));
      }
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(remountHidden ? 2 : 3);
    },
  );

  test("ends a saved interval after remount while hidden and preserves a zero-length visible interval", async () => {
    let now = 0;
    let hidden = false;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
    const stop = setup();
    now = 120_000;
    stop();
    hidden = true;
    now += 600_000;
    setup();
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(
      JSON.parse(await fetchMock.mock.calls[1]![0].text()).properties
        .duration_bucket,
    ).toBe("1_to_5m");
    hidden = false;
    globalThis.dispatchEvent(new Event("pageshow"));
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(
      JSON.parse(await fetchMock.mock.calls[2]![0].text()).properties
        .duration_bucket,
    ).toBe("under_1m");
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
