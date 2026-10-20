import {
  startBrowserTelemetry,
  type BrowserTelemetryReporter,
} from "@kenn-io/kit-ui/utils/browser-telemetry";
import { authenticatedFetch } from "../api/session";
import { getBasePath } from "../base-path";

const telemetryEventsPath = "/api/telemetry/events";
type CoreAction = "review_viewed" | "review_closed";
let reporter: BrowserTelemetryReporter | undefined;
let quiet = true;
// A review_viewed the daemon settled is not sent again on this page until the next UTC day.
let reviewViewedDay = "";
const reviewPending = new Set<string>();
const today = (): string => new Date().toISOString().slice(0, 10);
let listening = false;
let shellMounted = false;
let waitingForShell: Promise<void> | undefined;
let resumeReporting: (() => void) | undefined;
let visibleTime = 0;
let started: number | undefined;
let hasInterval = false;
let intervalEnded = false;
let hiddenAt: number | undefined;
let hiddenTimer: ReturnType<typeof setTimeout> | undefined;
export type Screen = "reviews" | "analytics";

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
  if (!shellMounted || quiet || !intervalEnded || !hasInterval) return;
  const duration = durationBucket(visibleTime);
  visibleTime = 0;
  hasInterval = false;
  intervalEnded = false;
  reporter?.reportVisit(
    {
      event: "session_ended",
      properties: { surface: "web", duration_bucket: duration },
    },
    { keepalive: true },
  );
}

// An opt-out discards what the session measured, wherever the page is in its check.
function discardSession(): void {
  clearTimeout(hiddenTimer);
  hiddenAt = undefined;
  pauseSession();
  visibleTime = 0;
  hasInterval = false;
  intervalEnded = false;
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
    !quiet &&
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
    if (!quiet) hiddenTimer = setTimeout(endSession, 1_800_000);
  } else {
    clearTimeout(hiddenTimer);
    if (hiddenAt !== undefined && Date.now() - hiddenAt >= 1_800_000)
      endSession();
    hiddenAt = undefined;
    resumeSession();
  }
}

// UIs at different base paths on one origin share local storage, so each keeps its own receipts.
// Encoding "." keeps the scope from running into the key segments after it.
function storageKey(): string {
  const scope = encodeURIComponent(getBasePath()).replaceAll(".", "%2E");
  return scope ? `roborev.telemetry.${scope}` : "roborev.telemetry";
}

function startReporting(): BrowserTelemetryReporter {
  return startBrowserTelemetry({
    route: telemetryEventsPath,
    surface: "web",
    storageKey: storageKey(),
    post: async (route, event, options) => {
      const readDay = today();
      if (event.event === "review_viewed") reviewPending.add(readDay);
      try {
        // kit-ui has no pause API; keep its receipts while deferring the network.
        while (!shellMounted) {
          waitingForShell ??= new Promise((resolve) => {
            resumeReporting = resolve;
          });
          await waitingForShell;
        }
        const response = await authenticatedFetch()(route, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(event),
          ...options,
          signal: AbortSignal.timeout(10_000),
        });
        const data: unknown = await response.json().catch(() => undefined);
        const receipt =
          response.status === 202 &&
          typeof data === "object" &&
          data !== null &&
          "status" in data
            ? data.status
            : undefined;
        if (receipt === "disabled") {
          discardSession();
        } else if (
          event.event === "review_viewed" &&
          (receipt === "queued" || receipt === "skipped") &&
          readDay > reviewViewedDay
        )
          reviewViewedDay = readDay;
        // Auth and rate-limit rejections never reached capture and can recover.
        const retryable = [401, 403, 429].includes(response.status);
        return { status: retryable ? 503 : response.status, data };
      } finally {
        if (event.event === "review_viewed") reviewPending.delete(readDay);
      }
    },
  });
}

export function reportScreenViewed(screen: Screen): void {
  if (shellMounted) reporter?.reportScreen(screen);
}

/** Reports app_opened on the page's first shell mount and on the first window focus of each later UTC day; returns a cleanup. */
export function setupAppOpenedReporting(): () => void {
  if (!listening) {
    listening = true;
    document.addEventListener("visibilitychange", onVisibility);
    globalThis.addEventListener("pagehide", endSession);
    globalThis.addEventListener("pageshow", resumeSession);
  }
  shellMounted = true;
  resumeReporting?.();
  resumeReporting = undefined;
  waitingForShell = undefined;
  if (!reporter) {
    reporter = startReporting();
    reporter.onQuietChange((value) => {
      quiet = value;
      if (quiet) {
        // A pending check only pauses the timer; a disabled receipt discards the session.
        clearTimeout(hiddenTimer);
        pauseSession();
      } else {
        flushSession();
        resumeSession();
      }
    });
  }
  flushSession();
  resumeSession();
  return () => {
    // The hidden timestamp and timer stay, so a long absence still ends the interval.
    shellMounted = false;
    pauseSession();
  };
}

export function reportCoreAction(event: CoreAction): void {
  const day = today();
  if (quiet || !shellMounted) return;
  if (
    event === "review_viewed" &&
    (reviewViewedDay === day || reviewPending.has(day))
  )
    return;
  reporter?.reportVisit({ event, properties: { surface: "web" } });
}
