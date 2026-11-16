package daemon

import (
	"context"
	"log"

	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

// reviewPathCensusForJob lists the paths supplied to a committed review. It
// returns nil for jobs without a committed diff and when measurement fails.
func reviewPathCensusForJob(
	ctx context.Context,
	repoPath string,
	job *storage.ReviewJob,
	excludes []string,
) *prompt.ReviewPathCensus {
	if job == nil {
		return nil
	}
	if job.JobType == "" || job.IsDirtyJob() {
		return nil
	}
	census, err := prompt.TakeReviewPathCensus(ctx, repoPath, job.GitRef, excludes)
	if err != nil {
		log.Printf("review file coverage: path census failed for job %d: %v", job.ID, err)
		return nil
	}
	return &census
}

// reviewFileCoverageForJob measures the paths supplied to a committed review.
// Measurement failures leave the informational metadata unknown.
func reviewFileCoverageForJob(
	ctx context.Context,
	repoPath string,
	job *storage.ReviewJob,
	excludes []string,
) *storage.ReviewFileCoverage {
	return coverageFromCensus(reviewPathCensusForJob(ctx, repoPath, job, excludes))
}

func coverageFromCensus(census *prompt.ReviewPathCensus) *storage.ReviewFileCoverage {
	if census == nil {
		return nil
	}
	return coverageFromSets(pathSet(census.Total), pathSet(census.Reviewed))
}

func pathSet(paths []string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			set[path] = struct{}{}
		}
	}
	return set
}

func coverageFromSets(total, reviewed map[string]struct{}) *storage.ReviewFileCoverage {
	excluded := 0
	for path := range total {
		if _, ok := reviewed[path]; !ok {
			excluded++
		}
	}
	reviewedCount := len(reviewed)
	return &storage.ReviewFileCoverage{Reviewed: &reviewedCount, Excluded: &excluded}
}

// failUnreviewableCensus fails the job when its census shows nothing to
// review. An agent shown an empty diff reports no issues, which reads as a
// pass. Retrying or failing over cannot change the diff, so the job fails
// without either. A nil census (unmeasured) lets the review run.
func (wp *WorkerPool) failUnreviewableCensus(
	ctx context.Context, workerID string, job *storage.ReviewJob,
	census *prompt.ReviewPathCensus, binaryEvidence bool,
) bool {
	if census == nil {
		return false
	}
	emptyErr := census.Reviewable(ctx, binaryEvidence)
	if emptyErr == nil {
		return false
	}
	wp.runAttemptTransition(workerID, job, func() {
		wp.failNonRetryableJobLocked(workerID, job, job.Agent, emptyErr.Error())
	})
	return true
}
