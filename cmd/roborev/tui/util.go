package tui

import (
	"fmt"
	"strings"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
)

func shortRef(ref string) string {
	if before, after, ok := strings.Cut(ref, ".."); ok {
		return gitrepo.ShortSHA(before) + ".." + gitrepo.ShortSHA(after)
	}
	return gitrepo.ShortSHA(ref)
}

func shortJobRef(job storage.ReviewJob) string {
	if job.CommitID == nil && job.DiffContent == nil {
		if job.GitRef == "prompt" {
			return "run"
		}
		if !strings.Contains(job.GitRef, "..") {
			return job.GitRef
		}
	}
	return shortRef(job.GitRef)
}

func formatAgentLabel(agent string, model string) string {
	if model != "" {
		return fmt.Sprintf("%s: %s", agent, model)
	}
	return agent
}

// detachedBranchLabel formats a placeholder branch label for a review job
// whose branch is empty because the commit was made on top of a detached
// HEAD (for example, mid git-bisect) rather than because the branch concept
// simply does not apply. A blank branch cell in the queue reads as a bug
// (see issue #499), so this surfaces the commit instead.
//
// Returns "" (leaving the caller's existing blank/fallback behavior intact)
// when: a branch (or the branchNone sentinel) is already stored; the job is
// neither a single-commit review nor a fix job on a commit (task, insights,
// and compact jobs carry non-SHA refs like "analyze", and dirty jobs have
// no branch concept); the job is a CI review, which deliberately leaves
// Branch empty in favor of CIBaseBranch (see storage.ReviewJob.HookBranch);
// or the ref is a range, "dirty", or "prompt" rather than a single commit.
// Fix jobs additionally require a SHA-like ref, since fixes created from
// task or custom-prompt parents reuse the parent's prompt-word ref.
func detachedBranchLabel(job storage.ReviewJob) string {
	if job.Branch != "" || job.IsDirtyJob() || job.IsCIReview() {
		return ""
	}
	if !job.IsReviewJob() && !job.IsFixJob() {
		return ""
	}
	ref := strings.TrimSpace(job.GitRef)
	if ref == "" || ref == "dirty" || ref == "prompt" || strings.Contains(ref, "..") {
		return ""
	}
	if job.IsFixJob() && !isCommitSHA(ref) {
		return ""
	}
	return detachedLabelPrefix + gitrepo.ShortSHA(ref) + ")"
}

// isCommitSHA reports whether ref looks like an abbreviated or full commit
// SHA (hex, 7-40 chars). Used to tell fix jobs reviewing a commit apart
// from fix jobs created off task/custom-prompt parents, whose ref is a
// prompt word like "analyze".
func isCommitSHA(ref string) bool {
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, c := range ref {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// detachedLabelPrefix opens every detachedBranchLabel result; filters use it
// to keep the display placeholder out of branch identity.
const detachedLabelPrefix = "(detached @ "

// isDetachedLabel reports whether a branch display value is the detached
// placeholder rather than a real branch name.
func isDetachedLabel(branch string) bool {
	return strings.HasPrefix(branch, detachedLabelPrefix)
}
