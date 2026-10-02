import { roborevFetch } from "../api/generated-fetch";

const telemetryEventsPath = "/api/telemetry/events";
let lastReportedDay = "";

function reportAppOpened(): void {
  const day = new Date().toISOString().slice(0, 10);
  if (day === lastReportedDay) return;
  lastReportedDay = day;
  void roborevFetch(telemetryEventsPath, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ event: "app_opened" }),
  }).catch(() => undefined);
}

/** Reports app_opened now and on the first window focus of each later UTC day; returns a cleanup. */
export function setupAppOpenedReporting(): () => void {
  reportAppOpened();
  globalThis.addEventListener("focus", reportAppOpened);
  return () => globalThis.removeEventListener("focus", reportAppOpened);
}
