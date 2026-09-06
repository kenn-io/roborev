package daemon

import (
	"context"
	"log"

	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

// reviewFileCoverageForJob measures the paths supplied to a committed review.
// Measurement failures leave the informational metadata unknown.
func reviewFileCoverageForJob(
	ctx context.Context,
	repoPath string,
	job *storage.ReviewJob,
	excludes []string,
) *storage.ReviewFileCoverage {
	if job == nil {
		return nil
	}
	if job.JobType == "" || job.IsDirtyJob() {
		return nil
	}
	reviewed, err := git.DiffPathsCtx(ctx, repoPath, job.GitRef, git.ReviewPathspecArgs(excludes...))
	if err != nil {
		log.Printf("review file coverage: reviewed path census failed for job %d: %v", job.ID, err)
		return nil
	}
	total, err := git.DiffPathsCtx(ctx, repoPath, job.GitRef, []string{"."})
	if err != nil {
		log.Printf("review file coverage: total path census failed for job %d: %v", job.ID, err)
		return nil
	}
	return coverageFromSets(pathSet(total), pathSet(reviewed))
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
