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
    vi.useRealTimers();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  test("keeps the first screen when reported before shell setup", async () => {
    appOpened.reportScreenViewed("analytics");
    setup();
    await settle();
    expect(
      await Promise.all(
        fetchMock.mock.calls.map(([request]) => request.clone().json()),
      ),
    ).toEqual([
      { event: "app_opened", properties: { surface: "web" } },
      {
        event: "screen_viewed",
        properties: { surface: "web", screen: "analytics" },
      },
    ]);
  });

  test.each(["pagehide", "hidden timeout"])(
    "flushes across UTC midnight without opening a hidden day on %s",
    async (ending) => {
      vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
      vi.setSystemTime(new Date("2026-03-10T23:45:00Z"));
      let hidden = false;
      vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
      setup();
      appOpened.reportScreenViewed("reviews");
      await vi.advanceTimersByTimeAsync(0);
      hidden = true;
      document.dispatchEvent(new Event("visibilitychange"));
      if (ending === "hidden timeout")
        await vi.advanceTimersByTimeAsync(1_800_000);
      else {
        vi.setSystemTime(new Date("2026-03-11T00:01:00Z"));
        globalThis.dispatchEvent(new Event("pagehide"));
        await vi.advanceTimersByTimeAsync(0);
      }
      const requests = fetchMock.mock.calls.map(([request]) => request);
      expect(
        await Promise.all(requests.map((request) => request.clone().json())),
      ).toEqual([
        { event: "app_opened", properties: { surface: "web" } },
        {
          event: "screen_viewed",
          properties: { surface: "web", screen: "reviews" },
        },
        {
          event: "session_ended",
          properties: { surface: "web", duration_bucket: "under_1m" },
        },
      ]);
      expect(requests[2]!.keepalive).toBe(true);
      hidden = false;
      focusWindow();
      await vi.advanceTimersByTimeAsync(0);
      expect(
        await Promise.all(
          fetchMock.mock.calls
            .slice(3)
            .map(([request]) => request.clone().json()),
        ),
      ).toEqual([
        { event: "app_opened", properties: { surface: "web" } },
        {
          event: "screen_viewed",
          properties: { surface: "web", screen: "reviews" },
        },
      ]);
    },
  );

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

  test("a disabled session receipt silences subsequent actions and sessions", async () => {
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    setup();
    await settle();
    fetchMock.mockImplementation(async () =>
      Response.json({ status: "disabled" }, { status: 202 }),
    );
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();
    appOpened.reportCoreAction("search_empty");
    appOpened.reportScreenViewed("analytics");
    globalThis.dispatchEvent(new Event("pageshow"));
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();
    expect(
      await Promise.all(
        fetchMock.mock.calls.map(([request]) => request.clone().json()),
      ),
    ).toEqual([
      { event: "app_opened", properties: { surface: "web" } },
      {
        event: "session_ended",
        properties: { surface: "web", duration_bucket: "under_1m" },
      },
    ]);
  });

  test.each([
    ["initial", "queued"],
    ["initial", "disabled"],
    ["initial", "unauthorized"],
    ["daily", "queued"],
    ["daily", "disabled"],
    ["daily", "unauthorized"],
  ])(
    "holds a completed session during a pending %s check until %s",
    async (check, receipt) => {
      let now = 0;
      let hidden = false;
      vi.spyOn(performance, "now").mockImplementation(() => now);
      vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
      if (check === "daily") {
        setup();
        await settle();
        vi.setSystemTime(new Date("2026-03-11T00:00:00Z"));
      }
      let resolveCheck!: (response: Response) => void;
      fetchMock.mockImplementationOnce(
        () => new Promise((resolve) => (resolveCheck = resolve)),
      );
      if (check === "initial") setup();
      else focusWindow();
      now = 120_000;
      hidden = true;
      document.dispatchEvent(new Event("visibilitychange"));
      globalThis.dispatchEvent(new Event("pagehide"));
      globalThis.dispatchEvent(new Event("pagehide"));
      await settle();
      const checks = check === "initial" ? 1 : 2;
      expect(fetchMock).toHaveBeenCalledTimes(checks);
      resolveCheck(
        receipt === "unauthorized"
          ? new Response(null, { status: 401 })
          : Response.json({ status: receipt }, { status: 202 }),
      );
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(
        checks + (receipt === "queued" ? 1 : 0),
      );
      if (receipt === "queued") {
        const request = fetchMock.mock.calls[checks]![0];
        expect(await request.json()).toEqual({
          event: "session_ended",
          properties: { surface: "web", duration_bucket: "1_to_5m" },
        });
        expect(request.keepalive).toBe(true);
      } else {
        // A later successful mount must not replay an opted-out interval.
        setup();
        await settle();
        expect(
          await Promise.all(
            fetchMock.mock.calls.map(([request]) => request.clone().json()),
          ),
        ).not.toContainEqual(
          expect.objectContaining({ event: "session_ended" }),
        );
      }
    },
  );

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

  test("sends one app_opened across ten page loads and asks again the next day", async () => {
    for (let load = 0; load < 10; load++) {
      setup();
      await settle();
      for (const cleanup of cleanups.splice(0)) cleanup();
      for (const [target, type, listener] of listeners.splice(0))
        target.removeEventListener(type, listener);
      vi.resetModules();
      appOpened = await import("./app-opened");
    }
    expect(fetchMock).toHaveBeenCalledTimes(1);
    vi.setSystemTime(new Date("2026-03-11T00:00:00Z"));
    setup();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test("a disabled receipt silences screens, reads, and pagehide until the next day's focus", async () => {
    fetchMock.mockImplementation(async () =>
      Response.json({ status: "disabled" }, { status: 202 }),
    );
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    setup();
    await settle();
    appOpened.reportScreenViewed("analytics");
    appOpened.reportCoreAction("review_viewed");
    globalThis.dispatchEvent(new Event("pagehide"));
    focusWindow();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    vi.setSystemTime(new Date("2026-03-11T00:00:00Z"));
    focusWindow();
    await settle();
    expect(
      fetchMock.mock.calls.filter(([request]) =>
        request.url.endsWith("/api/telemetry/events"),
      ),
    ).toHaveLength(2);
    expect(await fetchMock.mock.calls[1]![0].json()).toEqual({
      event: "app_opened",
      properties: { surface: "web" },
    });
  });

  test("reports each screen and review read once a day across focus returns", async () => {
    setup();
    appOpened.reportScreenViewed("reviews");
    await settle();
    for (let visit = 0; visit < 3; visit++) {
      appOpened.reportScreenViewed("analytics");
      appOpened.reportCoreAction("review_viewed");
      focusWindow();
      await settle();
    }
    const bodies = await Promise.all(
      fetchMock.mock.calls.map(([request]) => request.clone().json()),
    );
    expect(bodies).toEqual([
      { event: "app_opened", properties: { surface: "web" } },
      {
        event: "screen_viewed",
        properties: { surface: "web", screen: "reviews" },
      },
      {
        event: "screen_viewed",
        properties: { surface: "web", screen: "analytics" },
      },
      { event: "review_viewed", properties: { surface: "web" } },
    ]);
    vi.setSystemTime(new Date("2026-03-11T00:00:00Z"));
    focusWindow();
    await settle();
    appOpened.reportCoreAction("review_viewed");
    await settle();
    const reads = await Promise.all(
      fetchMock.mock.calls.map(([request]) => request.clone().json()),
    );
    expect(reads.filter((body) => body.event === "review_viewed")).toHaveLength(
      2,
    );
  });

  test("scopes requests and stored receipts to the base path", async () => {
    const meta = document.createElement("meta");
    meta.name = "roborev-base-path";
    meta.content = "/first";
    document.head.append(meta);
    setup();
    await settle();
    for (const cleanup of cleanups.splice(0)) cleanup();
    for (const [target, type, listener] of listeners.splice(0))
      target.removeEventListener(type, listener);
    meta.content = "/second";
    vi.resetModules();
    appOpened = await import("./app-opened");
    setup();
    await settle();
    expect(
      fetchMock.mock.calls.map(([request]) => new URL(request.url).pathname),
    ).toEqual(["/first/api/telemetry/events", "/second/api/telemetry/events"]);
    expect(
      Object.keys(localStorage).some((key) =>
        key.startsWith("roborev.telemetry.%2Ffirst."),
      ),
    ).toBe(true);
    expect(
      Object.keys(localStorage).some((key) =>
        key.startsWith("roborev.telemetry.%2Fsecond."),
      ),
    ).toBe(true);
  });

  test("a 401 allows a new check after signing back in and remounting", async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 401 }));
    const unmount = setup();
    await settle();
    unmount();
    sessionStorage.setItem("roborev.web.session", "recovered-session");
    sessionStorage.setItem("roborev.web.csrf", "recovered-csrf");
    setup();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    const request = fetchMock.mock.calls[1]![0];
    expect(request.headers.get("X-Roborev-Web-Session")).toBe(
      "recovered-session",
    );
    expect(request.headers.get("X-Roborev-CSRF")).toBe("recovered-csrf");
    expect(await request.json()).toEqual({
      event: "app_opened",
      properties: { surface: "web" },
    });
  });
});
