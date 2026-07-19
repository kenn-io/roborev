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
// not a single-commit review (task, insights, compact, and fix jobs can
// carry non-SHA refs like "analyze", and dirty jobs have no branch
// concept); the job is a CI review, which deliberately leaves Branch empty
// in favor of CIBaseBranch (see storage.ReviewJob.HookBranch); or the ref
// is a range, "dirty", or "prompt" rather than a single commit.
func detachedBranchLabel(job storage.ReviewJob) string {
	if job.Branch != "" || !job.IsReviewJob() || job.IsDirtyJob() || job.IsCIReview() {
		return ""
	}
	ref := strings.TrimSpace(job.GitRef)
	if ref == "" || ref == "dirty" || ref == "prompt" || strings.Contains(ref, "..") {
		return ""
	}
	return detachedLabelPrefix + gitrepo.ShortSHA(ref) + ")"
}

// detachedLabelPrefix opens every detachedBranchLabel result; filters use it
// to keep the display placeholder out of branch identity.
const detachedLabelPrefix = "(detached @ "

// isDetachedLabel reports whether a branch display value is the detached
// placeholder rather than a real branch name.
func isDetachedLabel(branch string) bool {
	return strings.HasPrefix(branch, detachedLabelPrefix)
}
