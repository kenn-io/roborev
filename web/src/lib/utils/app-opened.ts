import {
  startBrowserTelemetry,
  type BrowserTelemetryReporter,
} from "@kenn-io/kit-ui/utils/browser-telemetry";
import { getBasePath } from "../base-path";
import { authenticatedFetch } from "../api/session";

const telemetryEventsPath = "/api/telemetry/events";
let reporter: BrowserTelemetryReporter | undefined;
let quiet = true;
let sessionEnabled = true;
let pendingSessions: string[] = [];
type CoreAction = "review_viewed" | "review_closed";
const coreActionEvents = new Set<CoreAction>([
  "review_viewed",
  "review_closed",
]);
// kit-ui gates visits but leaves product action deduplication to the caller.
const coreActionClaims = new Set<string>();
const coreActionHistory = new Map<string, string[]>();
let coreActionPageOnly = false;
const pendingCoreActions = new Map<CoreAction, string>();
const coreActionClaimPrefix = "roborev.telemetry.action.v1.";
let listening = false;
let shellMounted = false;
let shellReady: Promise<void> | undefined;
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
  if (!shellMounted || !intervalEnded || !hasInterval) return;
  const duration = durationBucket(visibleTime);
  visibleTime = 0;
  hasInterval = false;
  intervalEnded = false;
  if (sessionEnabled) pendingSessions.push(duration);
  flushPendingSessions();
}

function flushPendingSessions(): void {
  if (quiet || !shellMounted) return;
  for (const duration of pendingSessions.splice(0)) {
    reporter?.reportVisit(
      {
        event: "session_ended",
        properties: { surface: "web", duration_bucket: duration },
      },
      { keepalive: true },
    );
  }
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
    sessionEnabled &&
    !document.hidden &&
    !intervalEnded &&
    started === undefined
  ) {
    hasInterval = true;
    started = performance.now();
  }
}

function onVisibility(): void {
  if (!sessionEnabled) return;
  if (!shellMounted) {
    if (document.hidden) endSession();
    return;
  }
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

type ActionClaim = {
  event: CoreAction;
  day: string;
  key: string;
  prefix: string;
};

// UIs at different base paths on one origin share local storage, so each keeps its own receipts.
// Encoding "." keeps the scope from running into the key segments after it.
function basePathScope(): string {
  return encodeURIComponent(getBasePath()).replaceAll(".", "%2E");
}

function coreActionKeyPrefix(event: CoreAction): string {
  const scope = basePathScope();
  return coreActionClaimPrefix + (scope && scope + ".") + event + ":";
}

function claimCoreAction(event: CoreAction): ActionClaim | null {
  const day = new Date().toISOString().slice(0, 10);
  const prefix = coreActionKeyPrefix(event);
  const key = prefix + day;
  let history = coreActionHistory.get(prefix) ?? [];
  try {
    if (!coreActionPageOnly)
      history = Object.keys(localStorage)
        .filter((key) => key.startsWith(prefix))
        .map((key) => ({
          day: key.slice(prefix.length),
          rank: Number(localStorage.getItem(key)),
        }))
        .sort((a, b) => a.rank - b.rank)
        .map(({ day }) => day);
    coreActionHistory.set(prefix, history);
  } catch {
    // Page memory still covers blocked storage.
  }
  if (history.includes(day) || coreActionClaims.has(key)) return null;
  coreActionClaims.add(key);
  return { event, day, key, prefix };
}

function settleCoreAction(claim: ActionClaim): void {
  const history = [
    ...(coreActionHistory.get(claim.prefix) ?? []),
    claim.day,
  ].slice(-35);
  coreActionHistory.set(claim.prefix, history);
  try {
    if (coreActionPageOnly) return;
    const previous = Object.keys(localStorage)
      .filter((key) => key.startsWith(claim.prefix) && key !== claim.key)
      .map((key) => ({ key, rank: Number(localStorage.getItem(key)) }))
      .sort((a, b) => a.rank - b.rank);
    localStorage.setItem(
      claim.key,
      String(Math.max(0, ...previous.map(({ rank }) => rank)) + 1),
    );
    for (const { key } of previous.slice(0, -34)) localStorage.removeItem(key);
  } catch {
    coreActionPageOnly = true;
  } finally {
    releaseCoreActionClaim(claim);
  }
}

function startReporting(): BrowserTelemetryReporter {
  return startBrowserTelemetry({
    route: telemetryEventsPath,
    surface: "web",
    storageKey: basePathScope()
      ? `roborev.telemetry.${basePathScope()}`
      : "roborev.telemetry",
    post: async (route, event, options) => {
      const sendingDay = new Date().toISOString().slice(0, 10);
      // kit-ui has no pause API; keep its receipts while deferring the network.
      while (!shellMounted) {
        shellReady ??= new Promise((resolve) => {
          resumeReporting = resolve;
        });
        await shellReady;
      }
      if (sendingDay !== new Date().toISOString().slice(0, 10)) {
        return { status: 503, data: undefined };
      }
      const action = coreActionEvents.has(event.event as CoreAction)
        ? claimCoreAction(event.event as CoreAction)
        : undefined;
      if (action === null) return { status: 202, data: { status: "skipped" } };
      let response: Response;
      try {
        response = await authenticatedFetch()(route, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(event),
          ...options,
          signal: AbortSignal.timeout(10_000),
        });
      } catch (error) {
        if (action) releaseCoreActionClaim(action);
        throw error;
      }
      const data: unknown = await response.json().catch(() => undefined);
      const status =
        typeof data === "object" && data !== null && "status" in data
          ? data.status
          : undefined;
      const accepted =
        response.status === 202 &&
        (status === "queued" || status === "skipped" || status === "disabled");
      if (accepted && status === "disabled") {
        pendingSessions = [];
        pendingCoreActions.clear();
        sessionEnabled = false;
      }
      const retryable =
        response.status === 401 ||
        response.status === 403 ||
        response.status === 429;
      if (action) {
        if (
          response.status === 202 ||
          (!retryable && response.status >= 400 && response.status < 500)
        ) {
          settleCoreAction(action);
        } else {
          releaseCoreActionClaim(action);
        }
      }
      // Auth and rate-limit rejections have not reached capture and can recover.
      return { status: retryable ? 503 : response.status, data };
    },
  });
}

export function reportScreenViewed(screen: Screen): void {
  if (!shellMounted) return;
  reporter?.reportScreen(screen);
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
  shellReady = undefined;
  if (!reporter) {
    reporter = startReporting();
    reporter.onQuietChange((value) => {
      quiet = value;
      if (quiet) {
        sessionEnabled = false;
        clearTimeout(hiddenTimer);
        hiddenTimer = undefined;
        hiddenAt = undefined;
        pauseSession();
        if (shellMounted) {
          visibleTime = 0;
          hasInterval = false;
          intervalEnded = false;
          pendingSessions = [];
        }
      } else {
        sessionEnabled = true;
        resumeSession();
      }
      flushPendingSessions();
      flushPendingCoreActions();
    });
  }
  flushPendingSessions();
  flushPendingCoreActions();
  flushSession();
  resumeSession();
  return () => {
    shellMounted = false;
    // The reporter owns page receipts, including those storage could not save.
    clearTimeout(hiddenTimer);
    hiddenAt = undefined;
    pauseSession();
  };
}

export function reportCoreAction(event: CoreAction): void {
  if (!shellMounted) return;
  const day = new Date().toISOString().slice(0, 10);
  if (quiet) {
    pendingCoreActions.set(event, day);
    return;
  }
  reporter?.reportVisit({ event, properties: { surface: "web" } });
}

function releaseCoreActionClaim(claim: ActionClaim): void {
  coreActionClaims.delete(claim.key);
}

function flushPendingCoreActions(): void {
  if (quiet || !shellMounted) return;
  for (const [event, day] of pendingCoreActions) {
    pendingCoreActions.delete(event);
    if (day === new Date().toISOString().slice(0, 10)) {
      reporter?.reportVisit({ event, properties: { surface: "web" } });
    }
  }
}
