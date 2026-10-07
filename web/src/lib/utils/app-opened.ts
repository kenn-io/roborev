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
let hiddenAt: number | undefined;
let hiddenTimer: ReturnType<typeof setTimeout> | undefined;
export type Screen = "reviews" | "analytics";
let currentScreen: (() => Screen) | undefined;

function pauseSession(): void {
  if (started === undefined) return;
  visibleTime += performance.now() - started;
  started = undefined;
}

function durationBucket(ms: number): string {
  return ms < 60_000
    ? "under_1m"
    : ms < 300_000
      ? "1_to_5m"
      : ms <= 1_800_000
        ? "5_to_30m"
        : "over_30m";
}

function flushSession(): void {
  if (!shellMounted || !intervalEnded || !hasInterval) return;
  const duration = durationBucket(visibleTime);
  visibleTime = 0;
  hasInterval = false;
  intervalEnded = false;
  void postEvent(
    "session_ended",
    { surface: "web", duration_bucket: duration },
    { keepalive: true },
  ).catch(() => undefined);
}

function endSession(): void {
  clearTimeout(hiddenTimer);
  hiddenAt = undefined;
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
  if (document.hidden) {
    pauseSession();
    hiddenAt = Date.now();
    clearTimeout(hiddenTimer);
    hiddenTimer = setTimeout(endSession, 1_800_000);
  } else {
    clearTimeout(hiddenTimer);
    if (hiddenAt !== undefined && Date.now() - hiddenAt >= 1_800_000)
      endSession();
    hiddenAt = undefined;
    resumeSession();
  }
}

function reportAppOpened(): boolean {
  const day = new Date().toISOString().slice(0, 10);
  if (day === lastReportedDay) return false;
  lastReportedDay = day;
  void postEvent("app_opened", { surface: "web" }).catch(() => undefined);
  return true;
}

function postEvent(
  event: string,
  properties: Record<string, string>,
  init: { signal?: AbortSignal; keepalive?: boolean } = {},
): Promise<unknown> {
  return roborevFetch(telemetryEventsPath, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ event, properties }),
    ...init,
  });
}

export function reportScreenViewed(screen: Screen): void {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 10_000);
  void postEvent(
    "screen_viewed",
    { screen, surface: "web" },
    { signal: controller.signal },
  )
    .catch(() => undefined)
    .finally(() => clearTimeout(timeout));
}

// Without a mounted shell there is no session to post with, so the focus waits for the shell to return.
function onFocus(): void {
  if (shellMounted) {
    if (reportAppOpened() && currentScreen) reportScreenViewed(currentScreen());
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
  return () => {
    shellMounted = false;
    pauseSession();
    currentScreen = undefined;
  };
}
