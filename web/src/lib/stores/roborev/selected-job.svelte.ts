import type { ReviewJob } from "../../api/generated/models";

// The selected job plus its last known subject. A reload clears the job so
// its stale status can't offer actions, but the tab keeps naming the review.
export function createSelectedJob() {
  let job = $state.raw<ReviewJob | null>(null);
  let known = $state.raw<{ id: number; subject: string } | null>(null);
  return {
    get job() {
      return job;
    },
    set(next: ReviewJob | null): void {
      job = next;
      if (next?.commit_subject)
        known = { id: next.id, subject: next.commit_subject };
    },
    subject(id: number | undefined): string | undefined {
      return known?.id === id ? known?.subject : undefined;
    },
  };
}
