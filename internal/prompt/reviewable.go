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
	return ReviewPathCensus{Ref: gitRef, Total: total, Reviewed: reviewed}, nil
}

// Reviewable returns a *NoReviewableChangesError when the review prompt would
// carry nothing for an agent to review: no diff after exclusions and no
// dependency metadata summary. Dependency metadata counts as content because
// the prompt lists those files even though their bodies are excluded.
func (c ReviewPathCensus) Reviewable() error {
	if len(c.Reviewed) > 0 || HasDependencyMetadataFiles(c.Total) {
		return nil
	}
	return &NoReviewableChangesError{Ref: c.Ref, Excluded: len(c.Total)}
}

// NoReviewableChangesError reports a committed review subject whose diff is
// empty once exclusions apply. An agent given an empty diff answers that it
// found no issues, which reads exactly like a review that passed, so callers
// fail the review instead of running the agent.
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
		return fmt.Sprintf("nothing to review: %s has an empty diff", ref)
	case 1:
		return fmt.Sprintf("nothing to review: the only changed file in %s is %s", ref, reason)
	default:
		return fmt.Sprintf("nothing to review: all %d changed files in %s are %s", e.Excluded, ref, reason)
	}
}
