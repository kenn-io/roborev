import {
  startBrowserTelemetry,
  type BrowserTelemetryOptions,
  type BrowserTelemetryQuietReason,
  type BrowserTelemetryReporter,
} from "@kenn-io/kit-ui/utils/browser-telemetry";
import { authenticatedFetch } from "../api/session";
import { getBasePath } from "../base-path";

const telemetryEventsPath = "/api/telemetry/events";
let reporter: BrowserTelemetryReporter | undefined;
let state: BrowserTelemetryQuietReason = "checking";
let listening = false;
let shellMounted = false;
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
  if (!intervalEnded || !hasInterval || state === "checking") return;
  if (!shellMounted && state === "enabled") return;
  const duration = durationBucket(visibleTime);
  visibleTime = 0;
  hasInterval = false;
  intervalEnded = false;
  if (state !== "enabled") return;
  void postTelemetry(
    telemetryEventsPath,
    {
      event: "session_ended",
      properties: { surface: "web", duration_bucket: duration },
    },
    { keepalive: true },
  )
    .then(async (response) => {
      if (
        response.status === 202 &&
        (await response.json()).status === "disabled"
      )
        reporter?.disable();
    })
    .catch(() => {});
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

// Scope receipts to each UI mounted on this origin.
function storageKey(): string {
  const scope = encodeURIComponent(getBasePath()).replaceAll(".", "%2E");
  return scope ? `roborev.telemetry.${scope}` : "roborev.telemetry";
}

const postTelemetry = ((route, event, options) =>
  authenticatedFetch()(route, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(event),
    ...options,
    signal: AbortSignal.timeout(10_000),
  })) satisfies BrowserTelemetryOptions["post"];

function startReporting(): BrowserTelemetryReporter {
  const reporting = startBrowserTelemetry({
    route: telemetryEventsPath,
    surface: "web",
    storageKey: storageKey(),
    post: postTelemetry,
  });
  reporting.onQuietChange((_quiet, reason) => {
    state = reason;
    flushSession();
    if (reason === "enabled") resumeSession();
  });
  return reporting;
}

export function reportScreenViewed(screen: Screen): void {
  getReporter().reportScreen(screen);
}

function getReporter(): BrowserTelemetryReporter {
  return (reporter ??= startReporting());
}

/** Starts session tracking and daily reporting on load or a visible return; returns a cleanup. */
export function setupAppOpenedReporting(): () => void {
  if (!listening) {
    listening = true;
    document.addEventListener("visibilitychange", onVisibility);
    globalThis.addEventListener("pagehide", endSession);
    globalThis.addEventListener("pageshow", resumeSession);
  }
  shellMounted = true;
  if (state === "unavailable") {
    reporter?.stop();
    reporter = undefined;
  }
  getReporter();
  flushSession();
  resumeSession();
  return () => {
    shellMounted = false;
    pauseSession();
  };
}

export function reportCoreAction(
  event: "review_viewed" | "review_closed" | "search_empty",
): void {
  const report = { event, properties: { surface: "web" } };
  if (event === "review_viewed") getReporter().reportDaily(event, report);
  else getReporter().reportVisit(report);
}

export function reportErrorShown(
  kind: "review_viewed" | "review_closed",
): void {
  getReporter().reportVisit({
    event: "error_shown",
    properties: { surface: "web", kind },
  });
}
