package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

const maxInsightsReviews = 100

func (s *Server) buildInsightsPrompt(
	ctx context.Context, repoRoot, branch string, since time.Time,
) (string, int, error) {
	asOf := time.Now()
	reviews, err := s.fetchInsightsReviews(repoRoot, branch, since, asOf)
	if err != nil {
		return "", 0, err
	}
	prior, err := s.fetchPriorInsights(repoRoot, branch, asOf)
	if err != nil {
		return "", 0, err
	}
	if len(reviews) == 0 && (prior == nil || prior.Suggestions == "" || strings.HasPrefix(prior.Suggestions, "No guideline change recommended")) {
		return "", 0, nil
	}

	measured, err := s.findingStats(ctx, storage.SummaryOptions{
		RepoPath: repoRoot, Branch: branch, Since: since, AsOf: asOf,
	}, "severity")
	if err != nil {
		return "", 0, fmt.Errorf("finding outcomes: %w", err)
	}
	cfg := s.configWatcher.Config()
	maxPromptSize := config.ResolveMaxPromptSize(repoRoot, cfg)
	guidelines := prompt.LoadGuidelinesWithConfig(ctx, repoRoot, cfg)

	promptText := prompt.BuildInsightsPrompt(prompt.InsightsData{
		Measured:      measured,
		Prior:         prior,
		Reviews:       reviews,
		Guidelines:    guidelines,
		RepoName:      filepath.Base(repoRoot),
		Since:         since,
		MaxPromptSize: maxPromptSize,
	})
	return promptText, len(reviews), nil
}

func (s *Server) fetchInsightsReviews(
	repoRoot, branch string, since time.Time, cutoff ...time.Time,
) ([]prompt.InsightsReview, error) {
	asOf := time.Now()
	if len(cutoff) > 0 {
		asOf = cutoff[0]
	}
	// Normalize to forward slashes to match how GetOrCreateRepo stores paths.
	repoFilter := filepath.ToSlash(repoRoot)

	listOpts := []storage.ListJobsOption{
		storage.WithoutPrompt(),
		storage.WithExcludePanelRole(storage.PanelRoleMember),
	}
	if branch != "" {
		listOpts = append(listOpts, storage.WithBranch(branch))
	}

	jobs, err := s.db.ListJobs(string(storage.JobStatusDone), repoFilter, 0, 0, listOpts...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	attachPanelSummaries(s.db, jobs)

	reviews := make([]prompt.InsightsReview, 0, min(len(jobs), maxInsightsReviews))
	for _, job := range jobs {
		if !job.IsReviewJob() && !job.IsSynthesisJob() || job.IsGoalReviewJob() {
			continue
		}
		if job.FinishedAt == nil || job.FinishedAt.Before(since) || job.FinishedAt.After(asOf) {
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

		// The joined job belongs to the loaded review version. Re-check it so a
		// rerun between ListJobs and this read cannot reuse stale eligibility.
		entry, eligible := currentInsightsReview(review, since, asOf)
		if !eligible {
			continue
		}
		if review.Job.IsSynthesisJob() && job.PanelSummary != nil {
			entry.PanelMembers = job.PanelSummary.MembersTotal
		}
		responses, err := s.db.GetCommentsForJob(job.ID)
		if err != nil {
			return nil, fmt.Errorf("get comments for job %d: %w", job.ID, err)
		}
		entry.Responses = storage.PromptTrustedResponses(responses)
		reviews = append(reviews, entry)
		if len(reviews) >= maxInsightsReviews {
			break
		}
	}

	return reviews, nil
}

// This runs before the new insights job is enqueued, so there is no current
// analysis ID to exclude. Exact branch equality is required even for "" (all).
func (s *Server) fetchPriorInsights(repoRoot, branch string, asOf time.Time) (*prompt.InsightsPrior, error) {
	options := []storage.ListJobsOption{storage.WithoutPrompt(), storage.WithJobType(storage.JobTypeInsights)}
	if branch == "" {
		options = append(options, storage.WithEmptyBranch())
	} else {
		options = append(options, storage.WithBranch(branch))
	}
	jobs, err := s.db.ListJobs(string(storage.JobStatusDone), filepath.ToSlash(repoRoot), 0, 0, options...)
	if err != nil {
		return nil, fmt.Errorf("list prior insights: %w", err)
	}
	var latest *storage.ReviewJob
	for i := range jobs {
		j := &jobs[i]
		if j.JobType != storage.JobTypeInsights || j.Branch != branch || j.FinishedAt == nil || !j.FinishedAt.Before(asOf) {
			continue
		}
		if latest == nil || compareInsightsCompletion(j, latest) > 0 {
			latest = j
		}
	}
	if latest == nil {
		return nil, nil
	}
	review, err := s.db.GetReviewByJobID(latest.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return &prompt.InsightsPrior{JobID: latest.ID, CompletedAt: *latest.FinishedAt}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get prior insights: %w", err)
	}
	if review.Job == nil || review.Job.Status != storage.JobStatusDone || review.Job.FinishedAt == nil || !review.Job.FinishedAt.Before(asOf) || !review.Job.FinishedAt.Equal(*latest.FinishedAt) {
		return nil, nil
	}
	job, err := s.db.GetJobByID(latest.ID)
	if err != nil {
		return nil, fmt.Errorf("get prior insights prompt: %w", err)
	}
	if job.Status != storage.JobStatusDone || job.FinishedAt == nil || !job.FinishedAt.Equal(*review.Job.FinishedAt) {
		return nil, nil
	}
	baseline := prompt.RecoverInsightsBaseline(job.Prompt)
	if baseline != nil {
		// completeJob stores insights completion at RFC3339 second precision.
		baselineAsOf := baseline.AsOf.Truncate(time.Second)
		finishedAt := latest.FinishedAt.Truncate(time.Second)
		if baseline.RepoPath != filepath.ToSlash(repoRoot) || baseline.Branch != branch || baselineAsOf.After(finishedAt) {
			baseline = nil
		}
	}
	return &prompt.InsightsPrior{
		JobID: latest.ID, CompletedAt: *latest.FinishedAt,
		Suggestions: prompt.RecoverInsightsSuggestions(review.Output), Baseline: baseline,
	}, nil
}

func compareInsightsCompletion(a, b *storage.ReviewJob) int {
	if c := a.FinishedAt.Compare(*b.FinishedAt); c != 0 {
		return c
	}
	// Prefer stable identities to local IDs for equal completion timestamps.
	au, bu := "", ""
	if a.UUID != nil {
		au = a.UUID.String()
	}
	if b.UUID != nil {
		bu = b.UUID.String()
	}
	if c := strings.Compare(au, bu); c != 0 {
		return c
	}
	if a.ID > b.ID {
		return 1
	}
	if a.ID < b.ID {
		return -1
	}
	return 0
}

func currentInsightsReview(review *storage.Review, since, asOf time.Time) (prompt.InsightsReview, bool) {
	j := review.Job
	if j == nil || j.Status != storage.JobStatusDone || j.FinishedAt == nil || j.FinishedAt.Before(since) || j.FinishedAt.After(asOf) || j.Verdict == nil || *j.Verdict != "F" {
		return prompt.InsightsReview{}, false
	}
	entry := prompt.InsightsReviewFromJob(*j, review.Output, nil, review.Closed)
	if review.UUID != nil {
		entry.ReviewUUID = review.UUID.String()
	}
	return entry, true
}
