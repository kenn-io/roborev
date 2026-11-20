import type { AppRoute } from "../router/router.svelte";

export const sessionTitles = {
  checking: "Checking · Roborev",
  login: "Connect · Roborev",
  error: "Unavailable · Roborev",
};

export function tabTitle(
  route: AppRoute,
  notFound: boolean,
  subject?: string,
): string {
  if (route.page === "analytics") return "Analytics · Roborev";
  if (route.jobId === undefined) return "Reviews · Roborev";
  if (notFound) return `Review not found (#${route.jobId}) · Roborev`;
  return `${subject ?? "Review"} (#${route.jobId}) · Roborev`;
}
