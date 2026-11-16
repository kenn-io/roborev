package prompt

import (
	"context"
	"fmt"

	"go.kenn.io/roborev/internal/git"
)

// ReviewPathCensus lists the paths a committed review subject changes and the
// subset that reaches the review diff once exclusion patterns apply.
type ReviewPathCensus struct {
	Ref      string
	Total    []string
	Reviewed []string

	repoPath string
	excludes []string
}

// TakeReviewPathCensus lists the changed paths of a commit or range, with and
// without the review's exclusion pathspecs. It uses the same path enumeration
// as the review diff, so an empty Reviewed list means an empty review diff.
func TakeReviewPathCensus(ctx context.Context, repoPath, gitRef string, excludes []string) (ReviewPathCensus, error) {
	reviewed, err := git.DiffPathsCtx(ctx, repoPath, gitRef, git.ReviewPathspecArgs(excludes...))
	if err != nil {
		return ReviewPathCensus{}, fmt.Errorf("list reviewed paths: %w", err)
	}
	total, err := git.DiffPathsCtx(ctx, repoPath, gitRef, []string{"."})
	if err != nil {
		return ReviewPathCensus{}, fmt.Errorf("list changed paths: %w", err)
	}
	return ReviewPathCensus{
		Ref: gitRef, Total: total, Reviewed: reviewed,
		repoPath: repoPath, excludes: excludes,
	}, nil
}

// Reviewable returns a *NoReviewableChangesError when the review prompt would
// carry nothing for an agent to review: no diff after exclusions, no
// dependency metadata summary, and, when binaryEvidence is on, no committed
// binary file. Dependency metadata counts because the prompt lists those files
// even though their bodies are excluded. Binary evidence compares a merge with
// its first parent, so a clean merge that brings in a binary file has evidence
// even though its combined diff is empty. A failure to list binary files lets
// the review run.
func (c ReviewPathCensus) Reviewable(ctx context.Context, binaryEvidence bool) error {
	if len(c.Reviewed) > 0 || HasDependencyMetadataFiles(c.Total) {
		return nil
	}
	if binaryEvidence {
		changes, err := git.ChangedBinaryFiles(ctx, c.repoPath, c.Ref, c.excludes...)
		if err != nil || len(changes) > 0 {
			return nil
		}
	}
	return &NoReviewableChangesError{Ref: c.Ref, Excluded: len(c.Total)}
}

// NoReviewableChangesError reports a committed review subject whose diff is
// empty once exclusions apply. An agent given an empty diff answers that it
// found no issues, which reads exactly like a review that passed, so callers
// skip the review instead of running the agent.
type NoReviewableChangesError struct {
	Ref string
	// Excluded counts changed paths that exclusion patterns removed.
	Excluded int
}

func (e *NoReviewableChangesError) Error() string {
	ref := git.ShortRef(e.Ref)
	const reason = "excluded from review by exclude_patterns or built-in exclusions"
	switch e.Excluded {
	case 0:
		return fmt.Sprintf("nothing to review in %s: the diff is empty", ref)
	case 1:
		return fmt.Sprintf("nothing to review in %s: the only changed file is %s", ref, reason)
	default:
		return fmt.Sprintf("nothing to review in %s: all %d changed files are %s", ref, e.Excluded, reason)
	}
}
