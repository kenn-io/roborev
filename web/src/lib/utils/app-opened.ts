import {
  startBrowserTelemetry,
  type BrowserTelemetryOptions,
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
let checks = 0;
type Post = BrowserTelemetryOptions["post"];
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

// A hidden interval ends 30 minutes after the tab hid, counted only while reporting is on.
function armHiddenTimer(): void {
  clearTimeout(hiddenTimer);
  if (quiet || hiddenAt === undefined) return;
  const left = hiddenAt + 1_800_000 - Date.now();
  if (left > 0) hiddenTimer = setTimeout(endSession, left);
  else endSession();
}

function onVisibility(): void {
  if (document.hidden) {
    pauseSession();
    hiddenAt = Date.now();
    armHiddenTimer();
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

// A post made while quiet is this tab's daily check; count the ones in flight.
function countChecks(post: Post): Post {
  return (...args) => {
    if (!quiet) return post(...args);
    checks++;
    return post(...args).finally(() => checks--);
  };
}

// A pending check keeps the paused session; quiet with no check in flight is an opt-out.
function discardUnlessChecking(): void {
  if (quiet && checks === 0) discardSession();
}

function onQuiet(value: boolean): void {
  quiet = value;
  armHiddenTimer();
  if (quiet) {
    pauseSession();
    queueMicrotask(discardUnlessChecking);
  } else {
    flushSession();
    resumeSession();
  }
}

function startReporting(): BrowserTelemetryReporter {
  return startBrowserTelemetry({
    route: telemetryEventsPath,
    surface: "web",
    storageKey: storageKey(),
    post: countChecks(async (route, event, options) => {
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
    }),
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
    reporter.onQuietChange(onQuiet);
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
