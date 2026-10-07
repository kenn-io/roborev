import { roborevFetch } from "../api/generated-fetch";

const telemetryEventsPath = "/api/telemetry/events";
let lastReportedDay = "";
let listening = false;
let shellMounted = false;
let loadReported = false;
let focusPending = false;
let visibleTime = 0;
let started: number | undefined;
let hasInterval = false;
let intervalEnded = false;
export type Screen = "reviews" | "analytics";
let currentScreen: (() => Screen) | undefined;
const screenDays = new Map<Screen, string>();

function pauseSession(): void {
  if (started === undefined) return;
  visibleTime += performance.now() - started;
  started = undefined;
}

function flushSession(): void {
  if (!shellMounted || !intervalEnded || !hasInterval) return;
  const duration =
    visibleTime < 60_000
      ? "under_1m"
      : visibleTime < 300_000
        ? "1_to_5m"
        : visibleTime <= 1_800_000
          ? "5_to_30m"
          : "over_30m";
  visibleTime = 0;
  hasInterval = false;
  intervalEnded = false;
  void roborevFetch(telemetryEventsPath, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      event: "session_ended",
      properties: { surface: "web", duration_bucket: duration },
    }),
    keepalive: true,
  }).catch(() => undefined);
}

function endSession(): void {
  pauseSession();
  if (!hasInterval) return;
  intervalEnded = true;
  flushSession();
}

function resumeSession(): void {
  if (
    shellMounted &&
    !document.hidden &&
    !intervalEnded &&
    started === undefined
  ) {
    hasInterval = true;
    started = performance.now();
  }
}

function onVisibility(): void {
  if (document.hidden) endSession();
  else resumeSession();
}

function reportAppOpened(): void {
  const day = new Date().toISOString().slice(0, 10);
  if (day === lastReportedDay) return;
  lastReportedDay = day;
  void postEvent("app_opened", { surface: "web" }).catch(() => undefined);
}

function postEvent(
  event: string,
  properties: Record<string, string>,
): Promise<unknown> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 10_000);
  return roborevFetch(telemetryEventsPath, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ event, properties }),
    signal: controller.signal,
  }).finally(() => clearTimeout(timeout));
}

export function reportScreenViewed(screen: Screen): void {
  if (!shellMounted) return;
  const day = new Date().toISOString().slice(0, 10);
  if (screenDays.get(screen) === day) return;
  screenDays.set(screen, day);
  void postEvent("screen_viewed", { screen, surface: "web" }).catch(() => {
    if (screenDays.get(screen) === day) screenDays.delete(screen);
  });
}

// Without a mounted shell there is no session to post with, so the focus waits for the shell to return.
function onFocus(): void {
  if (shellMounted) {
    reportAppOpened();
    if (currentScreen) reportScreenViewed(currentScreen());
  } else focusPending = true;
}

/** Reports app_opened on the page's first shell mount and on the first window focus of each later UTC day; returns a cleanup. */
export function setupAppOpenedReporting(getScreen?: () => Screen): () => void {
  if (!listening) {
    listening = true;
    globalThis.addEventListener("focus", onFocus);
    document.addEventListener("visibilitychange", onVisibility);
    globalThis.addEventListener("pagehide", endSession);
    globalThis.addEventListener("pageshow", resumeSession);
  }
  shellMounted = true;
  currentScreen = getScreen;
  // A remount after session recovery is not a load, so only a focus seen meanwhile makes it report.
  const report = !loadReported || focusPending;
  loadReported = true;
  focusPending = false;
  if (report) reportAppOpened();
  flushSession();
  resumeSession();
  if (getScreen) reportScreenViewed(getScreen());
  return () => {
    shellMounted = false;
    pauseSession();
    currentScreen = undefined;
  };
}
