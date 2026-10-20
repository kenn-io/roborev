import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import durationBuckets from "./duration-buckets.json";

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

function blockLocalStorageWrites() {
  const setItem = vi.fn(() => {
    throw new Error("blocked");
  });
  const target = window.localStorage;
  const storage = new Proxy(target, {
    get(target, property) {
      if (property === "setItem") return setItem;
      const value = Reflect.get(target, property, target);
      return typeof value === "function" ? value.bind(target) : value;
    },
  });
  vi.stubGlobal("localStorage", storage);
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: storage,
  });
  return setItem;
}

describe("setupAppOpenedReporting", () => {
  let appOpened: AppOpenedModule;
  let fetchMock: ReturnType<
    typeof vi.fn<(input: Request) => Promise<Response>>
  >;
  let windowLocalStorage: PropertyDescriptor | undefined;
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
    windowLocalStorage = Object.getOwnPropertyDescriptor(
      window,
      "localStorage",
    );
    vi.resetModules();
    appOpened = await import("./app-opened");
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-03-10T09:00:00Z"));
    localStorage.clear();
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
    if (windowLocalStorage)
      Object.defineProperty(window, "localStorage", windowLocalStorage);
    vi.restoreAllMocks();
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

  test.each(durationBuckets)(
    "reports $ms visible milliseconds as $bucket with closing credentials",
    async ({ ms: elapsed, bucket }) => {
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

  test("adds up visible time across 20 tab switches into one session", async () => {
    let now = 0;
    let hidden = false;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
    setup();
    await settle();
    for (let i = 0; i < 20; i++) {
      now += 60_000;
      hidden = true;
      document.dispatchEvent(new Event("visibilitychange"));
      now += 120_000;
      hidden = false;
      document.dispatchEvent(new Event("visibilitychange"));
    }
    expect(fetchMock).toHaveBeenCalledTimes(1);
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(JSON.parse(await fetchMock.mock.calls[1]![0].text())).toEqual({
      event: "session_ended",
      properties: { surface: "web", duration_bucket: "5_to_30m" },
    });
  });

  test.each(["timeout", "early return", "system sleep"])(
    "ends only after 30 hidden minutes, %s",
    async (scenario) => {
      vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
      let now = 0;
      let hidden = false;
      vi.spyOn(performance, "now").mockImplementation(() => now);
      vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
      setup();
      await vi.advanceTimersByTimeAsync(0);
      now = 120_000;
      hidden = true;
      document.dispatchEvent(new Event("visibilitychange"));
      if (scenario === "system sleep") {
        vi.setSystemTime(Date.now() + 31 * 60_000);
        expect(performance.now()).toBe(120_000);
      } else {
        await vi.advanceTimersByTimeAsync(1_799_999);
      }
      expect(fetchMock).toHaveBeenCalledTimes(1);
      if (scenario !== "timeout") {
        hidden = false;
        document.dispatchEvent(new Event("visibilitychange"));
      }
      if (scenario !== "system sleep") {
        await vi.advanceTimersByTimeAsync(1);
      }
      expect(fetchMock).toHaveBeenCalledTimes(
        scenario === "early return" ? 1 : 2,
      );
      if (scenario === "system sleep") now += 600_000;
      globalThis.dispatchEvent(new Event("pagehide"));
      expect(fetchMock).toHaveBeenCalledTimes(
        scenario === "system sleep" ? 3 : 2,
      );
      expect(
        JSON.parse(await fetchMock.mock.calls[1]![0].text()).properties
          .duration_bucket,
      ).toBe("1_to_5m");
      if (scenario === "system sleep") {
        expect(JSON.parse(await fetchMock.mock.calls[2]![0].text())).toEqual({
          event: "session_ended",
          properties: { surface: "web", duration_bucket: "5_to_30m" },
        });
      }
    },
  );

  test("starts on visible return and excludes hidden time and disposed mounts", async () => {
    let now = 0;
    let hidden = true;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
    const stop = setup();
    await settle();
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

  test.each([
    [false, false],
    [false, true],
    [true, false],
    [true, true],
  ])(
    "preserves recovery intervals, hide during gap=%s, remount hidden=%s",
    async (hideDuringGap, remountHidden) => {
      let now = 0;
      let hidden = false;
      vi.spyOn(performance, "now").mockImplementation(() => now);
      vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
      const stop = setup();
      await settle();
      now = 120_000;
      stop();
      if (hideDuringGap) {
        hidden = true;
        document.dispatchEvent(new Event("visibilitychange"));
        globalThis.dispatchEvent(new Event("pagehide"));
      }
      expect(fetchMock).toHaveBeenCalledTimes(1);
      now += 600_000;
      hidden = remountHidden;
      sessionStorage.setItem("roborev.web.session", "recovered-session");
      setup();
      if (!hideDuringGap) {
        if (!remountHidden) now += 10_000;
        globalThis.dispatchEvent(new Event("pagehide"));
      }
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(2);
      const request = fetchMock.mock.calls[1]![0];
      expect(request.headers.get("X-Roborev-Web-Session")).toBe(
        "recovered-session",
      );
      expect(JSON.parse(await request.text()).properties.duration_bucket).toBe(
        "1_to_5m",
      );
      if (hideDuringGap && remountHidden) {
        globalThis.dispatchEvent(new Event("pagehide"));
        await settle();
        expect(fetchMock).toHaveBeenCalledTimes(2);
      }
      if (!hideDuringGap && remountHidden) {
        hidden = false;
        globalThis.dispatchEvent(new Event("pageshow"));
        globalThis.dispatchEvent(new Event("pagehide"));
        await settle();
        expect(fetchMock).toHaveBeenCalledTimes(3);
        expect(
          JSON.parse(await fetchMock.mock.calls[2]![0].text()).properties
            .duration_bucket,
        ).toBe("under_1m");
      }
    },
  );

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
    const cleanup = setup();
    await settle();
    cleanup();
    setup();
    await settle();

    expect(fetchMock).toHaveBeenCalledTimes(1);
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

  test("persists daily receipts across page reloads", async () => {
    setup();
    await settle();
    appOpened.reportScreenViewed("reviews");
    await settle();
    for (const cleanup of cleanups.splice(0)) cleanup();
    for (const [target, type, listener] of listeners.splice(0))
      target.removeEventListener(type, listener);
    vi.resetModules();
    appOpened = await import("./app-opened");
    setup();
    appOpened.reportScreenViewed("reviews");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test("reports each core action once per UTC day across reloads", async () => {
    setup();
    await settle();
    appOpened.reportCoreAction("review_viewed");
    appOpened.reportCoreAction("review_viewed");
    appOpened.reportCoreAction("review_viewed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    for (const cleanup of cleanups.splice(0)) cleanup();
    for (const [target, type, listener] of listeners.splice(0))
      target.removeEventListener(type, listener);
    vi.resetModules();
    appOpened = await import("./app-opened");
    setup();
    appOpened.reportCoreAction("review_viewed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    focusWindow();
    await settle();
    appOpened.reportCoreAction("review_viewed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  test("scopes accepted action receipts to the configured base path", async () => {
    const meta = document.createElement("meta");
    meta.name = "roborev-base-path";
    meta.content = "/daemon-a";
    document.head.append(meta);

    setup();
    await settle();
    appOpened.reportCoreAction("review_viewed");
    await settle();

    for (const cleanup of cleanups.splice(0)) cleanup();
    for (const [target, type, listener] of listeners.splice(0))
      target.removeEventListener(type, listener);
    vi.resetModules();
    appOpened = await import("./app-opened");
    meta.content = "/daemon-b";
    setup();
    await settle();
    appOpened.reportCoreAction("review_viewed");
    await settle();

    const actionRequests = await Promise.all(
      fetchMock.mock.calls.map(async ([request]) => ({
        path: new URL(request.url).pathname,
        event: (JSON.parse(await request.clone().text()) as { event: string })
          .event,
      })),
    );
    expect(
      actionRequests
        .filter(({ event }) => event === "review_viewed")
        .map(({ path }) => path),
    ).toEqual([
      "/daemon-a/api/telemetry/events",
      "/daemon-b/api/telemetry/events",
    ]);
  });

  test("keeps an opt-out with the base path that received it", async () => {
    const meta = document.createElement("meta");
    meta.name = "roborev-base-path";
    meta.content = "/daemon-a";
    document.head.append(meta);
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ status: "disabled" }), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      }),
    );

    setup();
    await settle();

    for (const cleanup of cleanups.splice(0)) cleanup();
    for (const [target, type, listener] of listeners.splice(0))
      target.removeEventListener(type, listener);
    vi.resetModules();
    appOpened = await import("./app-opened");
    meta.content = "/daemon-b";
    setup();
    await settle();
    appOpened.reportScreenViewed("reviews");
    await settle();

    const requests = await Promise.all(
      fetchMock.mock.calls.map(async ([request]) => ({
        path: new URL(request.url).pathname,
        event: (JSON.parse(await request.clone().text()) as { event: string })
          .event,
      })),
    );
    expect(requests).toEqual([
      { path: "/daemon-a/api/telemetry/events", event: "app_opened" },
      { path: "/daemon-b/api/telemetry/events", event: "app_opened" },
      { path: "/daemon-b/api/telemetry/events", event: "screen_viewed" },
    ]);
  });

  test("releases a rejected daily action so later input can retry", async () => {
    vi.useFakeTimers();
    setup();
    await vi.advanceTimersByTimeAsync(0);
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 503 }));
    appOpened.reportCoreAction("review_closed");
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(1000);
    appOpened.reportCoreAction("review_closed");
    await vi.advanceTimersByTimeAsync(0);
    appOpened.reportCoreAction("review_closed");
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(JSON.parse(await fetchMock.mock.calls[2]![0].text()).event).toBe(
      "review_closed",
    );
  });

  test("reload retries a core action whose receipt has not arrived", async () => {
    setup();
    await settle();
    let resolveAction!: (response: Response) => void;
    fetchMock.mockImplementationOnce(
      () => new Promise((resolve) => (resolveAction = resolve)),
    );
    appOpened.reportCoreAction("review_closed");
    await settle();
    for (const cleanup of cleanups.splice(0)) cleanup();
    for (const [target, type, listener] of listeners.splice(0))
      target.removeEventListener(type, listener);
    vi.resetModules();
    appOpened = await import("./app-opened");
    setup();
    appOpened.reportCoreAction("review_closed");
    await settle();
    resolveAction(accepted());
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  test("queues core actions until the daemon accepts telemetry", async () => {
    let resolveAppOpened!: (response: Response) => void;
    fetchMock.mockImplementationOnce(
      () => new Promise((resolve) => (resolveAppOpened = resolve)),
    );
    setup();
    appOpened.reportCoreAction("review_viewed");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    resolveAppOpened(accepted());
    await settle();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(JSON.parse(await fetchMock.mock.calls[1]![0].text()).event).toBe(
      "review_viewed",
    );
  });

  test("disabled telemetry leaves no hidden-session timer", async () => {
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ status: "disabled" }), { status: 202 }),
    );
    const timeout = vi.spyOn(globalThis, "setTimeout");
    setup();
    await vi.advanceTimersByTimeAsync(0);
    vi.spyOn(document, "hidden", "get").mockReturnValue(true);
    document.dispatchEvent(new Event("visibilitychange"));

    expect(timeout.mock.calls.some(([, delay]) => delay === 1_800_000)).toBe(
      false,
    );
    await vi.advanceTimersByTimeAsync(30 * 60_000);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test("a disabled receipt cancels an existing hidden-session timer", async () => {
    vi.useFakeTimers();
    setup();
    await vi.advanceTimersByTimeAsync(0);
    vi.spyOn(document, "hidden", "get").mockReturnValue(true);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(vi.getTimerCount()).toBe(1);
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ status: "disabled" }), { status: 202 }),
    );
    appOpened.reportCoreAction("review_closed");
    await vi.advanceTimersByTimeAsync(0);
    expect(vi.getTimerCount()).toBe(0);
  });

  test("a disabled receipt quiets screens, actions, and session endings", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ status: "disabled" }), { status: 202 }),
    );
    setup();
    await settle();
    appOpened.reportScreenViewed("reviews");
    appOpened.reportCoreAction("review_viewed");
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    focusWindow();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  test("screen failures back off while navigation repeats", async () => {
    vi.useFakeTimers();
    setup();
    await vi.advanceTimersByTimeAsync(0);
    fetchMock.mockRejectedValueOnce(new TypeError("network down"));
    appOpened.reportScreenViewed("reviews");
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 100; i++) appOpened.reportScreenViewed("reviews");
    await vi.advanceTimersByTimeAsync(999);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });
  test("keeps a disabled receipt through auth recovery when local writes fail", async () => {
    const setItem = blockLocalStorageWrites();
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ status: "disabled" }), { status: 202 }),
    );
    const cleanup = setup();
    await settle();
    cleanup();
    setup();
    appOpened.reportScreenViewed("reviews");
    appOpened.reportCoreAction("review_viewed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(setItem).toHaveBeenCalled();
  });

  test("keeps action receipts in memory when only storage writes fail", async () => {
    const setItem = blockLocalStorageWrites();
    const cleanup = setup();
    await settle();
    appOpened.reportCoreAction("review_closed");
    await settle();
    cleanup();
    setup();
    appOpened.reportCoreAction("review_closed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(setItem).toHaveBeenCalled();
  });

  test("keeps accepted action days through a clock rollback and reload", async () => {
    setup();
    await settle();
    appOpened.reportCoreAction("review_closed");
    await settle();
    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    focusWindow();
    await settle();
    appOpened.reportCoreAction("review_closed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(4);
    for (const cleanup of cleanups.splice(0)) cleanup();
    for (const [target, type, listener] of listeners.splice(0))
      target.removeEventListener(type, listener);
    vi.resetModules();
    appOpened = await import("./app-opened");
    vi.setSystemTime(new Date("2026-03-10T09:00:00Z"));
    setup();
    appOpened.reportCoreAction("review_closed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  test("retains the latest 35 accepted action days", async () => {
    setup();
    await settle();
    for (let day = 1; day <= 36; day++) {
      vi.setSystemTime(new Date(Date.UTC(2026, 2, day)));
      focusWindow();
      await settle();
      appOpened.reportCoreAction("review_closed");
      await settle();
    }
    const actions = async () =>
      (
        await Promise.all(
          fetchMock.mock.calls.map(
            async ([request]) => JSON.parse(await request.clone().text()).event,
          ),
        )
      ).filter((event) => event === "review_closed").length;
    expect(await actions()).toBe(36);
    vi.setSystemTime(new Date("2026-03-02T00:00:00Z"));
    focusWindow();
    await settle();
    appOpened.reportCoreAction("review_closed");
    await settle();
    expect(await actions()).toBe(36);
    vi.setSystemTime(new Date("2026-03-01T00:00:00Z"));
    focusWindow();
    await settle();
    appOpened.reportCoreAction("review_closed");
    await settle();
    expect(await actions()).toBe(37);
  });

  test("bounds rejected action attempts through repeated input and reload", async () => {
    setup();
    await settle();
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 400 }));
    appOpened.reportCoreAction("review_closed");
    await settle();
    for (let i = 0; i < 100; i++) appOpened.reportCoreAction("review_closed");
    await settle();
    for (const cleanup of cleanups.splice(0)) cleanup();
    for (const [target, type, listener] of listeners.splice(0))
      target.removeEventListener(type, listener);
    vi.resetModules();
    appOpened = await import("./app-opened");
    setup();
    appOpened.reportCoreAction("review_closed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test("aborts a pending startup probe and retries after backoff", async () => {
    vi.useFakeTimers();
    vi.spyOn(AbortSignal, "timeout").mockImplementation((delay) => {
      const controller = new AbortController();
      setTimeout(() => controller.abort(), delay);
      return controller.signal;
    });
    fetchMock.mockImplementationOnce(
      (request) =>
        new Promise((_resolve, reject) => {
          request.signal.addEventListener("abort", () =>
            reject(request.signal.reason),
          );
        }),
    );
    setup();
    appOpened.reportScreenViewed("reviews");
    await vi.advanceTimersByTimeAsync(9999);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    await vi.advanceTimersByTimeAsync(1000);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(
      JSON.parse(await fetchMock.mock.calls[2]![0].text()).properties.screen,
    ).toBe("reviews");
  });
  test.each([false, true])(
    "pauses focus reporting during auth recovery with blocked storage=%s",
    async (blocked) => {
      if (blocked) {
        vi.spyOn(localStorage, "getItem").mockImplementation(() => {
          throw new Error("blocked");
        });
        vi.spyOn(localStorage, "setItem").mockImplementation(() => {
          throw new Error("blocked");
        });
      }
      let cleanup = setup();
      await settle();
      appOpened.reportScreenViewed("reviews");
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(2);
      cleanup();
      sessionStorage.clear();
      vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
      focusWindow();
      appOpened.reportScreenViewed("analytics");
      appOpened.reportCoreAction("review_viewed");
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(2);
      sessionStorage.setItem("roborev.web.session", "recovered-session");
      sessionStorage.setItem("roborev.web.csrf", "recovered-csrf");
      cleanup = setup();
      appOpened.reportScreenViewed("reviews");
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(4);
      expect(
        fetchMock.mock.calls[2]![0].headers.get("X-Roborev-Web-Session"),
      ).toBe("recovered-session");
      expect(
        JSON.parse(await fetchMock.mock.calls[3]![0].text()).properties.screen,
      ).toBe("reviews");
      cleanup();
      vi.setSystemTime(new Date("2026-03-10T09:00:00Z"));
      focusWindow();
      await settle();
      setup();
      appOpened.reportScreenViewed("reviews");
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(4);
    },
  );

  test("drops a paused future-day probe after rollback to an opted-out day", async () => {
    vi.spyOn(localStorage, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    vi.spyOn(localStorage, "setItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ status: "disabled" }), { status: 202 }),
    );
    const cleanup = setup();
    await settle();
    cleanup();
    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    focusWindow();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    vi.setSystemTime(new Date("2026-03-10T09:00:00Z"));
    setup();
    appOpened.reportScreenViewed("reviews");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test.each([401, 403, 429])(
    "retries rejected actions after recovery with backoff (%s)",
    async (status) => {
      vi.useFakeTimers();
      const cleanup = setup();
      await vi.advanceTimersByTimeAsync(0);
      const sessionLost = vi.fn(cleanup);
      globalThis.addEventListener("roborev:web-session-lost", sessionLost);
      fetchMock.mockResolvedValueOnce(new Response(null, { status }));
      appOpened.reportCoreAction("review_closed");
      await vi.advanceTimersByTimeAsync(0);
      expect(sessionLost).toHaveBeenCalledTimes(status === 401 ? 1 : 0);
      if (status === 401)
        expect(sessionStorage.getItem("roborev.web.session")).toBeNull();
      cleanup();
      sessionStorage.setItem("roborev.web.session", "recovered-session");
      sessionStorage.setItem("roborev.web.csrf", "recovered-csrf");
      setup();
      for (let i = 0; i < 100; i++) appOpened.reportCoreAction("review_closed");
      await vi.advanceTimersByTimeAsync(999);
      expect(fetchMock).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(1);
      appOpened.reportCoreAction("review_closed");
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchMock).toHaveBeenCalledTimes(3);
      expect(
        fetchMock.mock.calls[2]![0].headers.get("X-Roborev-Web-Session"),
      ).toBe("recovered-session");
      appOpened.reportCoreAction("review_closed");
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchMock).toHaveBeenCalledTimes(3);
    },
  );
  test.each([401, 403, 429])(
    "retries a rejected screen with recovered credentials (%s)",
    async (status) => {
      vi.useFakeTimers();
      const cleanup = setup();
      await vi.advanceTimersByTimeAsync(0);
      globalThis.addEventListener("roborev:web-session-lost", cleanup);
      fetchMock.mockResolvedValueOnce(new Response(null, { status }));
      appOpened.reportScreenViewed("reviews");
      await vi.advanceTimersByTimeAsync(0);
      cleanup();
      sessionStorage.setItem("roborev.web.session", "recovered-session");
      sessionStorage.setItem("roborev.web.csrf", "recovered-csrf");
      setup();
      appOpened.reportScreenViewed("reviews");
      await vi.advanceTimersByTimeAsync(999);
      expect(fetchMock).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(1);
      expect(fetchMock).toHaveBeenCalledTimes(3);
      const request = fetchMock.mock.calls[2]![0];
      expect(JSON.parse(await request.text()).properties.screen).toBe(
        "reviews",
      );
      expect(request.headers.get("X-Roborev-Web-Session")).toBe(
        "recovered-session",
      );
      appOpened.reportScreenViewed("reviews");
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchMock).toHaveBeenCalledTimes(3);
    },
  );
  test("retries an auth-rejected startup probe after login", async () => {
    vi.useFakeTimers();
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 401 }));
    const cleanup = setup();
    globalThis.addEventListener("roborev:web-session-lost", cleanup);
    await vi.advanceTimersByTimeAsync(1000);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    sessionStorage.setItem("roborev.web.session", "recovered-session");
    sessionStorage.setItem("roborev.web.csrf", "recovered-csrf");
    setup();
    appOpened.reportScreenViewed("reviews");
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(JSON.parse(await fetchMock.mock.calls[1]![0].text()).event).toBe(
      "app_opened",
    );
    expect(
      fetchMock.mock.calls[1]![0].headers.get("X-Roborev-Web-Session"),
    ).toBe("recovered-session");
    expect(
      JSON.parse(await fetchMock.mock.calls[2]![0].text()).properties.screen,
    ).toBe("reviews");
  });
  test("flushes a queued action when the startup receipt arrives during unmount", async () => {
    let resolveProbe!: (response: Response) => void;
    fetchMock.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveProbe = resolve;
        }),
    );
    const cleanup = setup();
    appOpened.reportCoreAction("review_viewed");
    cleanup();
    resolveProbe(accepted());
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    setup();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(JSON.parse(await fetchMock.mock.calls[1]![0].text())).toEqual({
      event: "review_viewed",
      properties: { surface: "web" },
    });
    appOpened.reportCoreAction("review_viewed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test.each(["", "invalid JSON", '{"status":"unknown"}'])(
    "settles an accepted action with an unreadable receipt: %s",
    async (body) => {
      setup();
      await settle();
      fetchMock.mockResolvedValueOnce(new Response(body, { status: 202 }));
      appOpened.reportCoreAction("review_closed");
      await settle();
      for (let i = 0; i < 100; i++) appOpened.reportCoreAction("review_closed");
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(JSON.parse(await fetchMock.mock.calls[1]![0].text()).event).toBe(
        "review_closed",
      );
    },
  );
});
