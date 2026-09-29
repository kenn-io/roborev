export function reviewTypeLabel(
  reviewType: string | undefined,
  panelRole?: string,
): string {
  if (panelRole === "synthesis") return "panel";
  if (
    reviewType === undefined ||
    reviewType === "" ||
    reviewType === "default" ||
    reviewType === "general" ||
    reviewType === "review"
  ) {
    return "default";
  }
  return reviewType;
}

/**
 * Job types the type column names beside the review type. Plain reviews need
 * no prefix, and a synthesis job's review type already reads "panel".
 */
export function displayedJobType(jobType: string): string | undefined {
  return jobType === "review" || jobType === "synthesis" ? undefined : jobType;
}

/** The text of the review table's type column, used for display and sort. */
export function reviewTypeColumnLabel(job: {
  job_type: string;
  review_type?: string;
  panel_role?: string;
}): string {
  const label = reviewTypeLabel(job.review_type, job.panel_role);
  const jobType = displayedJobType(job.job_type);
  return jobType === undefined ? label : `${jobType} · ${label}`;
}
