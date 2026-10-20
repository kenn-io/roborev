import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import durationBuckets from "./duration-buckets.json";
import { startBrowserTelemetry } from "@kenn-io/kit-ui/utils/browser-telemetry";

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

  async function sentEvents() {
    return Promise.all(
      fetchMock.mock.calls.map(async ([request]) => request.clone().json()),
    );
  }

  function startVisit() {
    let now = 0;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    return (ms: number) => void (now = ms);
  }

  test.each([
    [
      "a disabled reply discards the buffered session",
      async () => {
        const at = startVisit();
        setup();
        await settle();
        at(120_000);
        vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
        fetchMock.mockResolvedValueOnce(
          new Response(JSON.stringify({ status: "disabled" }), { status: 202 }),
        );
        focusWindow();
        await settle();
        vi.setSystemTime(new Date("2026-03-12T08:00:00Z"));
        focusWindow();
        await settle();
        at(130_000);
        globalThis.dispatchEvent(new Event("pagehide"));
        await settle();

        expect((await sentEvents()).at(-1)).toEqual({
          event: "session_ended",
          properties: { surface: "web", duration_bucket: "under_1m" },
        });
      },
    ],
    [
      "tracking restarts after a pagehide and pageshow during the check",
      async () => {
        const at = startVisit();
        setup();
        await settle();
        at(30_000);
        vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
        let answer!: (response: Response) => void;
        fetchMock.mockImplementationOnce(
          () => new Promise((resolve) => (answer = resolve)),
        );
        focusWindow();
        globalThis.dispatchEvent(new Event("pagehide"));
        globalThis.dispatchEvent(new Event("pageshow"));
        answer(accepted());
        await settle();
        at(150_000);
        globalThis.dispatchEvent(new Event("pagehide"));
        await settle();

        expect(
          (await sentEvents())
            .filter((e) => e.event === "session_ended")
            .map((e) => e.properties.duration_bucket),
        ).toEqual(["under_1m", "1_to_5m"]);
      },
    ],
    [
      "a pending check pauses the session without erasing it",
      async () => {
        const at = startVisit();
        setup();
        await settle();
        at(30_000);
        vi.setSystemTime(new Date("2026-03-11T00:01:00Z"));
        focusWindow();
        await settle();
        at(60_000);
        globalThis.dispatchEvent(new Event("pagehide"));
        await settle();

        expect((await sentEvents()).at(-1)).toEqual({
          event: "session_ended",
          properties: { surface: "web", duration_bucket: "1_to_5m" },
        });
      },
    ],
    [
      "hiding the tab during a pending check still ends a long absence as its own session",
      async () => {
        vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
        let now = 0;
        let hidden = false;
        vi.spyOn(performance, "now").mockImplementation(() => now);
        vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
        setup();
        await vi.advanceTimersByTimeAsync(0);
        now = 30_000;
        vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
        let answer!: (response: Response) => void;
        fetchMock.mockImplementationOnce(
          () => new Promise((resolve) => (answer = resolve)),
        );
        focusWindow();
        hidden = true;
        document.dispatchEvent(new Event("visibilitychange"));
        answer(accepted());
        await vi.advanceTimersByTimeAsync(31 * 60_000);
        hidden = false;
        now += 31 * 60_000;
        document.dispatchEvent(new Event("visibilitychange"));
        now += 10_000;
        globalThis.dispatchEvent(new Event("pagehide"));
        await vi.advanceTimersByTimeAsync(0);

        expect(
          (await sentEvents())
            .filter((e) => e.event === "session_ended")
            .map((e) => e.properties.duration_bucket),
        ).toEqual(["under_1m", "under_1m"]);
      },
    ],
    [
      "an enabled reply while hidden ends the session 30 minutes after the tab hid",
      async () => {
        vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
        let now = 0;
        let hidden = false;
        vi.spyOn(performance, "now").mockImplementation(() => now);
        vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
        setup();
        await vi.advanceTimersByTimeAsync(0);
        now = 120_000;
        vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
        let answer!: (response: Response) => void;
        fetchMock.mockImplementationOnce(
          () => new Promise((resolve) => (answer = resolve)),
        );
        focusWindow();
        hidden = true;
        document.dispatchEvent(new Event("visibilitychange"));
        await vi.advanceTimersByTimeAsync(10_000);
        answer(accepted());
        await vi.advanceTimersByTimeAsync(30 * 60_000 - 10_001);
        expect((await sentEvents()).at(-1)?.event).toBe("app_opened");
        await vi.advanceTimersByTimeAsync(1);

        expect((await sentEvents()).at(-1)).toEqual({
          event: "session_ended",
          properties: { surface: "web", duration_bucket: "1_to_5m" },
        });
      },
    ],
  ])("next-day check: %s", async (_, scenario) => scenario());

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
      expect(key).toMatch(/^roborev\.telemetry/);
    }
  });

  test("drops core actions while the daemon check is pending", async () => {
    let resolveAppOpened!: (response: Response) => void;
    fetchMock.mockImplementationOnce(
      () => new Promise((resolve) => (resolveAppOpened = resolve)),
    );
    setup();
    appOpened.reportCoreAction("review_viewed");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    resolveAppOpened(accepted());
    await settle();

    expect((await sentEvents()).map((e) => e.event)).toEqual(["app_opened"]);
    appOpened.reportCoreAction("review_closed");
    await settle();
    expect((await sentEvents()).map((e) => e.event)).toEqual([
      "app_opened",
      "review_closed",
    ]);
  });

  test("a cross-tab opt-out discards measured time before reporting resumes", async () => {
    const at = startVisit();
    setup();
    await settle();
    at(120_000);
    const otherTab = startBrowserTelemetry({
      route: "/api/telemetry/events",
      surface: "web",
      storageKey: "roborev.telemetry",
      post: async () => ({ status: 202, data: { status: "queued" } }),
    });
    otherTab.disable();
    otherTab.stop();
    focusWindow();
    await settle();
    vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
    focusWindow();
    await settle();
    at(130_000);
    globalThis.dispatchEvent(new Event("pagehide"));
    await settle();

    expect(
      (await sentEvents())
        .filter((event) => event.event === "session_ended")
        .map((event) => event.properties.duration_bucket),
    ).toEqual(["under_1m"]);
  });

  test.each(["disabled reply", "cross-tab quiet"])(
    "quiet receipts: %s",
    async (scenario) => {
      vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
      let now = 0;
      let hidden = false;
      vi.spyOn(performance, "now").mockImplementation(() => now);
      vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
      if (scenario === "disabled reply") {
        fetchMock.mockResolvedValueOnce(
          new Response(JSON.stringify({ status: "disabled" }), { status: 202 }),
        );
      }
      const timeout = vi.spyOn(globalThis, "setTimeout");
      setup();
      if (scenario === "disabled reply")
        appOpened.reportCoreAction("review_viewed");
      await vi.advanceTimersByTimeAsync(0);
      if (scenario === "cross-tab quiet") {
        now = 120_000;
        hidden = true;
        document.dispatchEvent(new Event("visibilitychange"));
        expect(vi.getTimerCount()).toBe(1);
        const otherTab = startBrowserTelemetry({
          route: "/api/telemetry/events",
          surface: "web",
          storageKey: "roborev.telemetry",
          post: async () => ({ status: 202, data: { status: "queued" } }),
        });
        otherTab.disable();
        otherTab.stop();
        focusWindow();
        // jsdom queues a zero-delay storage event when the other tab writes its receipt.
        await vi.advanceTimersByTimeAsync(0);
        expect(vi.getTimerCount()).toBe(0);
      } else {
        appOpened.reportCoreAction("review_closed");
        hidden = true;
        document.dispatchEvent(new Event("visibilitychange"));
        expect(
          timeout.mock.calls.some(([, delay]) => delay === 1_800_000),
        ).toBe(false);
      }
      if (scenario === "disabled reply") {
        appOpened.reportScreenViewed("reviews");
        globalThis.dispatchEvent(new Event("pagehide"));
        await vi.advanceTimersByTimeAsync(30 * 60_000);
        expect(fetchMock).toHaveBeenCalledTimes(1);
        return;
      }
      vi.setSystemTime(new Date("2026-03-11T08:00:00Z"));
      focusWindow();
      await vi.advanceTimersByTimeAsync(0);
      hidden = false;
      document.dispatchEvent(new Event("visibilitychange"));
      now += 10_000;
      globalThis.dispatchEvent(new Event("pagehide"));
      await vi.advanceTimersByTimeAsync(0);
      expect(
        (await sentEvents())
          .filter((e) => e.event === "session_ended")
          .map((e) => e.properties.duration_bucket),
      ).toEqual(["under_1m"]);
    },
  );

  test.each([
    "the reply comes the same day",
    "the reply comes after midnight",
    "an older review reply preserves the newer settled day",
    "a read's day is fixed when it is sent, so an older reply cannot settle a newer day",
    "coalesces pending review views and releases a failed read",
  ])("review views: %s", async (scenario) => {
    const olderReply =
      scenario === "an older review reply preserves the newer settled day";
    if (
      olderReply ||
      scenario ===
        "a read's day is fixed when it is sent, so an older reply cannot settle a newer day"
    ) {
      let now = 0;
      vi.spyOn(performance, "now").mockImplementation(() => now);
      vi.setSystemTime(new Date("2026-03-10T23:59:50Z"));
      setup();
      await settle();
      const answers: Array<(response: Response) => void> = [];
      fetchMock.mockImplementation(async (request) =>
        (await request.clone().json()).event === "review_viewed"
          ? new Promise((resolve) => answers.push(resolve))
          : accepted(),
      );
      appOpened.reportCoreAction("review_viewed");
      await settle();
      vi.setSystemTime(new Date("2026-03-11T00:00:05Z"));
      focusWindow();
      await settle();
      appOpened.reportCoreAction("review_viewed");
      await settle();
      expect(answers).toHaveLength(2);
      if (olderReply) {
        answers[1]!(accepted());
        await settle();
        answers[0]!(accepted());
      } else {
        answers[0]!(accepted());
        answers[1]!(new Response(null, { status: 500 }));
      }
      await settle();
      now += 60_000;
      appOpened.reportCoreAction("review_viewed");
      await settle();

      if (olderReply) {
        expect(
          (await sentEvents()).filter((e) => e.event === "review_viewed"),
        ).toHaveLength(2);
      } else {
        expect(answers).toHaveLength(3);
      }
      return;
    }
    if (
      scenario === "coalesces pending review views and releases a failed read"
    ) {
      let now = 0;
      vi.spyOn(performance, "now").mockImplementation(() => now);
      setup();
      await settle();
      let answer!: (response: Response) => void;
      fetchMock.mockImplementationOnce(
        () => new Promise((resolve) => (answer = resolve)),
      );
      for (let i = 0; i < 30; i++) {
        appOpened.reportCoreAction("review_viewed");
        focusWindow();
        document.dispatchEvent(new Event("visibilitychange"));
      }
      expect(fetchMock).toHaveBeenCalledTimes(2);
      answer(new Response(null, { status: 500 }));
      await settle();
      now = 60_000;
      appOpened.reportCoreAction("review_viewed");
      await settle();
      expect((await sentEvents()).map((e) => e.event)).toEqual([
        "app_opened",
        "review_viewed",
        "review_viewed",
      ]);
      return;
    }
    const lateReply = scenario === "the reply comes after midnight";
    vi.setSystemTime(new Date("2026-03-10T23:59:50Z"));
    setup();
    await settle();
    let answer!: (response: Response) => void;
    if (lateReply)
      fetchMock.mockImplementationOnce(
        () => new Promise((resolve) => (answer = resolve)),
      );
    appOpened.reportCoreAction("review_viewed");
    if (lateReply) {
      vi.setSystemTime(new Date("2026-03-11T00:00:05Z"));
      answer(accepted());
    } else {
      await settle();
      for (let i = 0; i < 5; i++) {
        appOpened.reportCoreAction("review_viewed");
        focusWindow();
      }
      appOpened.reportCoreAction("review_closed");
      appOpened.reportCoreAction("review_closed");
    }
    await settle();
    if (!lateReply) {
      const events = (await sentEvents()).map((e) => e.event);
      expect(events.filter((e) => e === "review_viewed")).toHaveLength(1);
      expect(events.filter((e) => e === "review_closed")).toHaveLength(2);
      vi.setSystemTime(new Date("2026-03-11T00:00:05Z"));
      focusWindow();
      await settle();
    }

    appOpened.reportCoreAction("review_viewed");
    await settle();
    expect(
      (await sentEvents()).filter((e) => e.event === "review_viewed"),
    ).toHaveLength(2);
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
});
