package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

const maxInsightsReviews = 100

func (s *Server) buildInsightsPrompt(
	ctx context.Context, repoRoot, branch string, since time.Time,
) (string, int, error) {
	reviews, err := s.fetchInsightsReviews(repoRoot, branch, since)
	if err != nil {
		return "", 0, err
	}
	if len(reviews) == 0 {
		return "", 0, nil
	}

	cfg := s.configWatcher.Config()
	maxPromptSize := config.ResolveMaxPromptSize(repoRoot, cfg)
	guidelines := prompt.LoadGuidelinesWithConfig(ctx, repoRoot, cfg)

	promptText := prompt.BuildInsightsPrompt(prompt.InsightsData{
		Reviews:       reviews,
		Guidelines:    guidelines,
		RepoName:      filepath.Base(repoRoot),
		Since:         since,
		MaxPromptSize: maxPromptSize,
	})
	return promptText, len(reviews), nil
}

func (s *Server) fetchInsightsReviews(
	repoRoot, branch string, since time.Time,
) ([]prompt.InsightsReview, error) {
	// Normalize to forward slashes to match how GetOrCreateRepo stores paths.
	repoFilter := filepath.ToSlash(repoRoot)

	var listOpts []storage.ListJobsOption
	if branch != "" {
		listOpts = append(listOpts, storage.WithBranch(branch))
	}

	jobs, err := s.db.ListJobs(string(storage.JobStatusDone), repoFilter, 0, 0, listOpts...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}

	reviews := make([]prompt.InsightsReview, 0, min(len(jobs), maxInsightsReviews))
	for _, job := range jobs {
		if !job.IsReviewJob() {
			continue
		}
		if job.FinishedAt != nil && job.FinishedAt.Before(since) {
			continue
		}
		if job.Verdict == nil || *job.Verdict != "F" {
			continue
		}

		review, err := s.db.GetReviewByJobID(job.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("get review for job %d: %w", job.ID, err)
		}

		responses, err := s.db.GetCommentsForJob(job.ID)
		if err != nil {
			return nil, fmt.Errorf("get comments for job %d: %w", job.ID, err)
		}

		reviews = append(reviews, prompt.InsightsReviewFromJob(
			job,
			review.Output,
			responses,
			review.Closed,
		))
		if len(reviews) >= maxInsightsReviews {
			break
		}
	}

	return reviews, nil
}
