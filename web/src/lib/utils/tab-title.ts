import type { ReviewJob } from "../api/generated/models";
import type { AppRoute } from "../router/router.svelte";

export function tabTitle(
  route: AppRoute,
  job: ReviewJob | null | undefined,
): string {
  if (route.page === "analytics") return "Analytics · Roborev";
  if (route.jobId === undefined) return "Reviews · Roborev";
  const selected = job?.id === route.jobId ? job : undefined;
  return [
    `${selected?.commit_subject || "Review"} (#${route.jobId})`,
    selected?.repo_name,
    "Roborev",
  ]
    .filter(Boolean)
    .join(" · ");
}
