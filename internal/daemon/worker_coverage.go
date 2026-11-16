package daemon

import (
	"context"
	"fmt"
	"log"

	"go.kenn.io/roborev/internal/config"
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

// skipUnreviewableCensus ends the job as skipped when its census shows nothing
// to review. Running the agent on an empty diff would store "No issues found",
// which reads as a review that passed. Nothing to review is not a failure
// either, so the job is skipped with the reason and no verdict. A nil census
// (unmeasured) lets the review run.
func (wp *WorkerPool) skipUnreviewableCensus(
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
	wp.skipJob(ctx, workerID, job, emptyErr.Error())
	return true
}

// skipJob ends the running attempt as skipped. Subscribers get a
// review.completed event without a verdict, and a skipped panel member counts
// as finished so its synthesis can run.
func (wp *WorkerPool) skipJob(ctx context.Context, workerID string, job *storage.ReviewJob, reason string) {
	wp.runAttemptTransition(workerID, job, func() {
		skipped, err := wp.db.SkipJob(ctx, job.ID, workerID, reason)
		if err != nil {
			log.Printf("[%s] Error skipping job %d: %v", workerID, job.ID, err)
			wp.failNonRetryableJobLocked(workerID, job, job.Agent, fmt.Sprintf("skip job: %v", err))
			return
		}
		if !skipped {
			return
		}
		log.Printf("[%s] Job %d %s skipped: %s", workerID, job.ID, job.RepoName, reason)
		wp.invalidateBudgetSpend()
		wp.broadcaster.Broadcast(eventForJob("review.completed", job, job.ID))
		wp.releaseIfPanelMember(job)
	})
}

// skipUnreviewableWithoutAgent skips an ordinary committed review whose diff
// is empty after exclusions, before the worker prepares a checkout or an
// agent. The worker uses it when the agent cannot run, so a review with
// nothing to review is skipped instead of failed for agent availability.
func (wp *WorkerPool) skipUnreviewableWithoutAgent(
	ctx context.Context, workerID string, job *storage.ReviewJob, cfg *config.Config,
) bool {
	if !job.IsReviewJob() || job.IsDirtyJob() || job.IsGoalReviewJob() || job.UsesStoredPrompt() ||
		job.PromptPrebuilt || job.Source == storage.JobSourceCI || job.Source == storage.JobSourceRebuttal {
		return false
	}
	// Validate the worktree like checkout preparation does: a stale path
	// inside the main checkout would narrow the census to that directory.
	repoPath := resolveEffectiveRepoPath(workerID, job)
	settings, err := reviewJobSettings(job, repoPath, cfg)
	if err != nil {
		return false
	}
	excludes := config.ResolveExcludePatterns(ctx, repoPath, cfg, job.ReviewType)
	census := reviewPathCensusForJob(ctx, repoPath, job, excludes)
	return wp.skipUnreviewableCensus(ctx, workerID, job, census, settings.BinaryEvidence)
}
