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

  function setup(getScreen?: () => "reviews" | "analytics"): () => void {
    const cleanup = appOpened.setupAppOpenedReporting(getScreen);
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

  test("reposts on focus after a successful screen response", async () => {
    setup(() => "reviews");
    await settle();
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ status: "queued" }), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      }),
    );
    appOpened.reportScreenViewed("reviews");
    await settle();
    focusWindow();
    vi.setSystemTime(new Date("2026-03-10T20:00:00Z"));
    focusWindow();
    await settle();

    expect(fetchMock).toHaveBeenCalledTimes(3);
    appOpened.reportScreenViewed("analytics");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(4);
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
    ["rejected", () => Promise.reject(new TypeError("network down")), false],
    [
      "screen rejected",
      () => Promise.reject(new TypeError("network down")),
      true,
    ],
    [
      "answered 400",
      async () => new Response("unsupported telemetry event", { status: 400 }),
      false,
    ],
    [
      "screen answered 400",
      async () => new Response("unsupported telemetry event", { status: 400 }),
      true,
    ],
  ])(
    "swallows a failed post (%s) and still reports the next day",
    async (_, fail, screenPost) => {
      const unhandled = vi.fn();
      process.on("unhandledRejection", unhandled);
      try {
        if (screenPost) {
          setup();
          await settle();
          fetchMock.mockImplementationOnce(fail);
          appOpened.reportScreenViewed("reviews");
        } else {
          fetchMock.mockImplementationOnce(fail);
          setup();
        }
        await settle();
        await settle();
        expect(unhandled).not.toHaveBeenCalled();

        vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
        if (screenPost) appOpened.reportScreenViewed("reviews");
        else focusWindow();
        await settle();
        expect(fetchMock).toHaveBeenCalledTimes(screenPost ? 3 : 2);
      } finally {
        process.off("unhandledRejection", unhandled);
      }
    },
  );

  test("holds a focus seen while the shell is gone until it returns", async () => {
    const mount = () => {
      const cleanup = appOpened.setupAppOpenedReporting(() => "analytics");
      appOpened.reportScreenViewed("analytics");
      cleanups.push(cleanup);
      return cleanup;
    };
    mount()();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    focusWindow();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    mount();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect(JSON.parse(await fetchMock.mock.calls[3]![0].text())).toEqual({
      event: "screen_viewed",
      properties: { screen: "analytics", surface: "web" },
    });
  });
  test("reports page changes and focus while the browser clock stays on the same day", async () => {
    let screen: "reviews" | "analytics" = "reviews";
    const cleanup = appOpened.setupAppOpenedReporting(() => screen);
    cleanups.push(cleanup);
    appOpened.reportScreenViewed("reviews");
    screen = "analytics";
    appOpened.reportScreenViewed(screen);
    appOpened.reportScreenViewed("reviews");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(3);
    appOpened.reportScreenViewed("reviews");
    await settle();
    focusWindow();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(5);
    const bodies = await Promise.all(
      fetchMock.mock.calls.map(async ([request]) =>
        JSON.parse(await request.text()),
      ),
    );
    expect(
      bodies
        .filter((body) => body.event === "screen_viewed")
        .map((body) => body.properties.screen),
    ).toEqual(["reviews", "analytics", "reviews", "analytics"]);
  });

  test("bounds screen requests while preserving app-open transport", async () => {
    vi.useFakeTimers();
    fetchMock.mockImplementation(
      (request) =>
        new Promise((_resolve, reject) =>
          request.signal.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          ),
        ),
    );
    cleanups.push(appOpened.setupAppOpenedReporting(() => "reviews"));
    appOpened.reportScreenViewed("reviews");
    await vi.advanceTimersByTimeAsync(10_000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0]![0].signal.aborted).toBe(false);
    expect(fetchMock.mock.calls[1]![0].signal.aborted).toBe(true);
  });

  test.each(["rejected", "answered 400"])(
    "waits for a pending screen before retrying a failed request (%s)",
    async (failure) => {
      setup(() => "reviews");
      await settle();
      let finish!: () => void;
      fetchMock.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve, reject) => {
            finish = () => {
              if (failure === "rejected") reject(new TypeError("offline"));
              else
                resolve(
                  new Response("unsupported telemetry event", { status: 400 }),
                );
            };
          }),
      );
      appOpened.reportScreenViewed("reviews");
      focusWindow();
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(2);
      finish();
      await settle();
      focusWindow();
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(3);
      focusWindow();
      await settle();
      expect(fetchMock).toHaveBeenCalledTimes(4);
    },
  );
});
