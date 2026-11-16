package daemon

import (
	"context"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
)

const ciDraftSkipReason = "Review skipped: draft"

func matchingCISkipLabel(prLabels, skipLabels []string) (string, bool) {
	for _, label := range prLabels {
		label = strings.TrimSpace(label)
		for _, configured := range skipLabels {
			configured = strings.TrimSpace(configured)
			if configured != "" && strings.EqualFold(label, configured) {
				return label, true
			}
		}
	}
	return "", false
}

// ciNothingToReviewSkipReason is the commit status for a PR whose changes give
// no panel member anything to review.
const ciNothingToReviewSkipReason = "Review skipped: nothing to review"

// ciNothingToReviewError reports that no panel member would see any change in
// the PR range, so the poller publishes a skip instead of enqueuing.
type ciNothingToReviewError struct {
	reason string
}

func (e *ciNothingToReviewError) Error() string { return e.reason }

func (e *ciNothingToReviewError) skip() ciSkip {
	return ciSkip{description: ciNothingToReviewSkipReason, reason: e.reason}
}

// ciNothingToReview returns a *ciNothingToReviewError when every member's
// review diff of gitRef is empty after that member's exclusions. An agent
// shown an empty diff reports no issues, which would publish a pass for a PR
// nobody reviewed. A census failure lets the run proceed.
func ciNothingToReview(
	ctx context.Context, repoPath, gitRef string,
	members []config.ResolvedMember, cfg *config.Config,
) error {
	var empty error
	for _, m := range members {
		excludes := config.ResolveExcludePatterns(ctx, repoPath, cfg, m.ReviewType)
		census, err := prompt.TakeReviewPathCensus(ctx, repoPath, gitRef, excludes)
		if err != nil {
			return nil
		}
		if empty = census.Reviewable(ctx, m.BinaryEvidence); empty == nil {
			return nil
		}
	}
	if empty == nil {
		return nil
	}
	return &ciNothingToReviewError{reason: empty.Error()}
}
