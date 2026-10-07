import { roborevFetch } from "../api/generated-fetch";

const telemetryEventsPath = "/api/telemetry/events";
let lastReportedDay = "";
let listening = false;
let shellMounted = false;
let loadReported = false;
let focusPending = false;

function reportAppOpened(): void {
  const day = new Date().toISOString().slice(0, 10);
  if (day === lastReportedDay) return;
  lastReportedDay = day;
  void roborevFetch(telemetryEventsPath, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      event: "app_opened",
      properties: { surface: "web" },
    }),
  }).catch(() => undefined);
}

// Without a mounted shell there is no session to post with, so the focus waits for the shell to return.
function onFocus(): void {
  if (shellMounted) reportAppOpened();
  else focusPending = true;
}

/** Reports app_opened on the page's first shell mount and on the first window focus of each later UTC day; returns a cleanup. */
export function setupAppOpenedReporting(): () => void {
  if (!listening) {
    listening = true;
    globalThis.addEventListener("focus", onFocus);
  }
  shellMounted = true;
  // A remount after session recovery is not a load, so only a focus seen meanwhile makes it report.
  const report = !loadReported || focusPending;
  loadReported = true;
  focusPending = false;
  if (report) reportAppOpened();
  let started = document.hidden ? undefined : performance.now();
  const end = () => {
    if (started === undefined) return;
    const elapsed = performance.now() - started;
    started = undefined;
    const duration =
      elapsed < 60_000
        ? "under_1m"
        : elapsed < 300_000
          ? "1_to_5m"
          : elapsed <= 1_800_000
            ? "5_to_30m"
            : "over_30m";
    void roborevFetch(telemetryEventsPath, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        event: "session_ended",
        properties: { surface: "web", duration_bucket: duration },
      }),
      keepalive: true,
    }).catch(() => undefined);
  };
  const resume = () => {
    if (!document.hidden && started === undefined) started = performance.now();
  };
  const visibility = () => (document.hidden ? end() : resume());
  document.addEventListener("visibilitychange", visibility);
  globalThis.addEventListener("pagehide", end);
  globalThis.addEventListener("pageshow", resume);
  return () => {
    shellMounted = false;
    started = undefined;
    document.removeEventListener("visibilitychange", visibility);
    globalThis.removeEventListener("pagehide", end);
    globalThis.removeEventListener("pageshow", resume);
  };
}
