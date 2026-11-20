import type { AppRoute } from "../router/router.svelte";

export function tabTitle(route: AppRoute, subject?: string): string {
  if (route.page === "analytics") return "Analytics · Roborev";
  if (route.jobId === undefined) return "Reviews · Roborev";
  return `${subject?.trim() || "Review"} (#${route.jobId}) · Roborev`;
}
