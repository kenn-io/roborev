package agenthook

import (
	"context"

	"go.kenn.io/roborev/internal/storage"
)

// ReviewSource supplies the tracked repository and review metadata needed by
// the Agent Hook state machine.
type ReviewSource interface {
	ResolveTrackedRepo(
		ctx context.Context, path, branch string,
	) (TrackedRepoResolution, bool)
	ListOpenReviewJobs(
		ctx context.Context, repoRoot, branch string,
	) ([]storage.ReviewJob, bool)
}
