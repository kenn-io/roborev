import type { components } from "../api/generated";
import type { SessionCapabilities } from "../api/session";
import { parseCostUsd } from "./roborev-usage";

type ReviewJob = components["schemas"]["ReviewJob"];

const TERMINAL_STATUSES = new Set([
  "done",
  "applied",
  "rebased",
  "failed",
  "canceled",
  "skipped",
]);

const RERUNNABLE_STATUSES = new Set(["done", "failed", "canceled", "skipped"]);

export function isTerminalStatus(status: string): boolean {
  return TERMINAL_STATUSES.has(status);
}

export function canRerunJob(job: ReviewJob): boolean {
  return (
    job.panel_role !== "member" &&
    !(job.status === "skipped" && job.source === "auto_design") &&
    RERUNNABLE_STATUSES.has(job.status)
  );
}

export function canCancelJob(
  job: ReviewJob,
  capabilities: SessionCapabilities,
): boolean {
  if (job.status !== "queued" && job.status !== "running") return false;
  if (capabilities.cancelAnyJob) return true;
  if (!capabilities.cancelReviewJob) return false;
  return isRemoteCancelableReview(job);
}

function isRemoteCancelableReview(job: ReviewJob): boolean {
  return (
    (job.job_type === "review" ||
      job.job_type === "range" ||
      job.job_type === "dirty") &&
    job.source !== "ci" &&
    !job.panel_role &&
    !job.panel_run_uuid &&
    !job.agentic &&
    !job.prompt_prebuilt
  );
}

export function isPanelParent(job: ReviewJob): boolean {
  return (
    job.panel_role === "synthesis" &&
    (job.panel_summary?.members_total ?? 0) > 0
  );
}

export function panelStatusLabel(job: ReviewJob): string | null {
  const summary = job.panel_summary;
  if (!isPanelParent(job) || !summary) return null;

  if (!isTerminalStatus(job.status)) {
    return `synthesizing… ${summary.members_terminal}/${summary.members_total} reviewers done`;
  }

  const parts: string[] = [];
  if (summary.members_succeeded > 0)
    parts.push(`${summary.members_succeeded} ok`);
  if (summary.members_failed > 0)
    parts.push(`${summary.members_failed} failed`);
  if (summary.members_canceled > 0)
    parts.push(`${summary.members_canceled} canceled`);
  if (summary.members_skipped > 0)
    parts.push(`${summary.members_skipped} skipped`);
  if (parts.length === 0)
    return `${summary.members_terminal}/${summary.members_total} done`;
  return parts.join(" · ");
}

export function panelCostUsd(
  job: ReviewJob,
  members: ReviewJob[] | undefined,
): number | null {
  const ownCost = parseCostUsd(job.token_usage);
  if (!isPanelParent(job)) return ownCost;

  let total = ownCost ?? 0;
  let hasAnyCost = ownCost !== null;

  if (members !== undefined) {
    for (const member of members) {
      const memberCost = parseCostUsd(member.token_usage);
      if (memberCost !== null) {
        total += memberCost;
        hasAnyCost = true;
      }
    }
    return hasAnyCost ? total : null;
  }

  const summary = job.panel_summary;
  if (
    summary &&
    ((summary.members_with_cost ?? 0) > 0 ||
      summary.members_cost_complete === true)
  ) {
    return total + (summary.members_cost_usd ?? 0);
  }

  return hasAnyCost ? total : null;
}

export function panelElapsedStart(
  job: ReviewJob,
  members: ReviewJob[] | undefined,
): string | undefined {
  let earliest = job.started_at;

  function consider(candidate: string | undefined): void {
    if (!candidate) return;
    if (
      !earliest ||
      new Date(candidate).getTime() < new Date(earliest).getTime()
    ) {
      earliest = candidate;
    }
  }

  if (isPanelParent(job)) {
    consider(job.panel_summary?.first_started_at);
    for (const member of members ?? []) consider(member.started_at);
  }

  return earliest;
}

export function panelReviewHeader(
  job: ReviewJob,
  members: ReviewJob[] | undefined,
): string | null {
  const summary = job.panel_summary;
  if (!isPanelParent(job) || !summary) return null;

  if (members !== undefined && members.length > 0) {
    const parts = members.map(
      (member) =>
        `${member.panel_member_name || member.agent} ${member.verdict ?? "·"}`,
    );
    return `${members.length} reviewers: ${parts.join(", ")}`;
  }

  const split = panelStatusLabel(job);
  return split ? `${summary.members_total} reviewers: ${split}` : null;
}
