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

function storedKeys(): string[] {
  return Array.from({ length: localStorage.length }, (_, i) =>
    localStorage.key(i),
  ).filter((key) => key !== null);
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

  test("a long hidden stretch across an unmount ends the earlier session", async () => {
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
    let now = 0;
    let hidden = false;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
    const stop = setup();
    await vi.advanceTimersByTimeAsync(0);
    now = 120_000;
    hidden = true;
    document.dispatchEvent(new Event("visibilitychange"));
    stop();
    await vi.advanceTimersByTimeAsync(31 * 60_000);
    hidden = false;
    now += 31 * 60_000;
    document.dispatchEvent(new Event("visibilitychange"));
    setup();
    now += 10_000;
    globalThis.dispatchEvent(new Event("pagehide"));
    await vi.advanceTimersByTimeAsync(0);

    const sessions = await Promise.all(
      fetchMock.mock.calls.slice(1).map(async ([request]) => request.json()),
    );
    expect(sessions.map((body) => body.properties.duration_bucket)).toEqual([
      "1_to_5m",
      "under_1m",
    ]);
  });

  test("keeps only kit-ui keys in local storage", async () => {
    setup();
    await settle();
    appOpened.reportScreenViewed("reviews");
    appOpened.reportCoreAction("review_viewed");
    appOpened.reportCoreAction("review_closed");
    await settle();

    expect(fetchMock).toHaveBeenCalledTimes(4);
    const keys = storedKeys();
    expect(keys.length).toBeGreaterThan(0);
    for (const key of keys) {
      expect(key).toMatch(/^roborev\.telemetry\.browser\.v1\./);
    }
  });

  test("sends review_viewed once per UTC day after the daemon settles it", async () => {
    setup();
    await settle();
    appOpened.reportCoreAction("review_viewed");
    await settle();
    for (let i = 0; i < 5; i++) {
      appOpened.reportCoreAction("review_viewed");
      focusWindow();
    }
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    appOpened.reportCoreAction("review_closed");
    appOpened.reportCoreAction("review_closed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(4);

    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    focusWindow();
    await settle();
    appOpened.reportCoreAction("review_viewed");
    await settle();
    const events = await Promise.all(
      fetchMock.mock.calls.map(async ([request]) => request.json()),
    );
    expect(events.filter((e) => e.event === "review_viewed")).toHaveLength(2);
  });

  test("a check after the UTC rollover pauses the session without erasing it", async () => {
    let now = 0;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    setup();
    await settle();
    now = 30_000;
    vi.setSystemTime(new Date("2026-03-11T00:01:00Z"));
    focusWindow();
    await settle();
    now += 30_000;
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();

    const events = await Promise.all(
      fetchMock.mock.calls.map(async ([request]) => request.json()),
    );
    expect(events.at(-1)).toEqual({
      event: "session_ended",
      properties: { surface: "web", duration_bucket: "1_to_5m" },
    });
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

  test("queued actions survive an authentication remount", async () => {
    const cleanup = setup();
    await settle();
    cleanup();
    appOpened.reportCoreAction("review_closed");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    setup();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(JSON.parse(await fetchMock.mock.calls[1]![0].text())).toEqual({
      event: "review_closed",
      properties: { surface: "web" },
    });
  });

  test("a disabled receipt drops queued actions and starts no visit timer", async () => {
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ status: "disabled" }), { status: 202 }),
    );
    const timeout = vi.spyOn(globalThis, "setTimeout");
    setup();
    appOpened.reportCoreAction("review_viewed");
    await vi.advanceTimersByTimeAsync(0);
    vi.spyOn(document, "hidden", "get").mockReturnValue(true);
    document.dispatchEvent(new Event("visibilitychange"));
    appOpened.reportScreenViewed("reviews");
    globalThis.dispatchEvent(new Event("pagehide"));
    await vi.advanceTimersByTimeAsync(30 * 60_000);

    expect(timeout.mock.calls.some(([, delay]) => delay === 1_800_000)).toBe(
      false,
    );
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test.each([401, 403, 429])(
    "a %s answer does not claim the app-open check",
    async (status) => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status }));
      setup();
      await settle();
      expect(storedKeys()).toEqual([]);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    },
  );

  test("reports each screen once per UTC day and survives a failed post", async () => {
    setup();
    await settle();
    fetchMock.mockRejectedValueOnce(new TypeError("network down"));
    appOpened.reportScreenViewed("reviews");
    await settle();
    appOpened.reportScreenViewed("analytics");
    await settle();
    appOpened.reportScreenViewed("analytics");
    await settle();

    const events = await Promise.all(
      fetchMock.mock.calls.map(async ([request]) => request.json()),
    );
    expect(events.map((body) => body.event)).toEqual([
      "app_opened",
      "screen_viewed",
    ]);
  });
});
