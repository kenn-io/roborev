import type { AppRoute } from "../router/router.svelte";

export type SessionScreen = "checking" | "login" | "error";

const sessionTitles: Record<SessionScreen, string> = {
  checking: "Checking · Roborev",
  login: "Connect · Roborev",
  error: "Unavailable · Roborev",
};

export function sessionTitle(screen: SessionScreen): string {
  return sessionTitles[screen];
}

export function tabTitle(route: AppRoute, notFound: boolean): string {
  if (route.page === "analytics") return "Analytics · Roborev";
  if (route.jobId === undefined) return "Reviews · Roborev";
  if (notFound) return `Review not found (#${route.jobId}) · Roborev`;
  return `Review (#${route.jobId}) · Roborev`;
}
