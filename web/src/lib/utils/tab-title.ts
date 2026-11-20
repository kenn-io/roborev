import type { ReviewJob } from "../api/generated/models";
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

export function tabTitle(
  route: AppRoute,
  job: ReviewJob | null | undefined,
  notFound: boolean,
): string {
  if (route.page === "analytics") return "Analytics · Roborev";
  if (route.jobId === undefined) return "Reviews · Roborev";
  const selected = job?.id === route.jobId ? job : undefined;
  if (!selected && notFound)
    return `Review not found (#${route.jobId}) · Roborev`;
  return [
    `${selected?.commit_subject || "Review"} (#${route.jobId})`,
    selected?.repo_name,
    "Roborev",
  ]
    .filter(Boolean)
    .join(" · ");
}
