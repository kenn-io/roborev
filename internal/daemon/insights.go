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
	prior, err := s.fetchPriorInsights(ctx, repoRoot, branch, asOf)
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
	repoRoot, branch string, since, asOf time.Time,
) ([]prompt.InsightsReview, error) {
	// Normalize to forward slashes to match how GetOrCreateRepo stores paths.
	repoFilter := filepath.ToSlash(repoRoot)

	listOpts := []storage.ListJobsOption{
		storage.WithoutPrompt(),
		storage.WithExcludePanelRole(storage.PanelRoleMember),
		storage.WithFailedReviewsInWindow(since, asOf),
	}
	if branch != "" {
		listOpts = append(listOpts, storage.WithBranch(branch))
	}

	jobs, err := s.db.ListJobs(string(storage.JobStatusDone), repoFilter, maxInsightsReviews, 0, listOpts...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	attachPanelSummaries(s.db, jobs)

	reviews := make([]prompt.InsightsReview, 0, min(len(jobs), maxInsightsReviews))
	for _, job := range jobs {
		// SQLite's window comparison has millisecond precision.
		if job.FinishedAt == nil || job.FinishedAt.Before(since) || job.FinishedAt.After(asOf) {
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
	}

	return reviews, nil
}

// This runs before the new insights job is enqueued, so there is no current
// analysis ID to exclude.
func (s *Server) fetchPriorInsights(ctx context.Context, repoRoot, branch string, asOf time.Time) (*prompt.InsightsPrior, error) {
	repoPath := filepath.ToSlash(repoRoot)
	options := []storage.ListJobsOption{storage.WithoutPrompt(), storage.WithJobType(storage.JobTypeInsights)}
	if branch == "" {
		options = append(options, storage.WithEmptyBranch())
	} else {
		options = append(options, storage.WithBranch(branch))
	}
	jobs, err := s.db.ListJobs(string(storage.JobStatusDone), repoPath, 0, 0, options...)
	if err != nil {
		return nil, fmt.Errorf("list prior insights: %w", err)
	}
	var latest, job *storage.ReviewJob
	var baseline *storage.FindingStats
	for i := range jobs {
		j := &jobs[i]
		if j.FinishedAt == nil || !j.FinishedAt.Before(asOf) || (latest != nil && compareInsightsCompletion(j, latest) <= 0) {
			continue
		}
		if j.Branch != "(none)" {
			latest, job, baseline = j, nil, nil
			continue
		}

		// Older TUIs backfilled empty scope with "(none)", which is also a
		// valid branch name. The stored prompt records the scope used to run.
		candidate, err := s.db.GetJobByIDContext(ctx, j.ID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get prior insights prompt: %w", err)
		}
		if candidate.Status != storage.JobStatusDone || candidate.FinishedAt == nil || !candidate.FinishedAt.Equal(*j.FinishedAt) {
			continue
		}
		scope := prompt.RecoverInsightsBaseline(candidate.Prompt)
		if scope != nil && scope.RepoPath == repoPath && scope.Branch == branch {
			latest, job, baseline = j, candidate, scope
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
	if job == nil {
		job, err = s.db.GetJobByIDContext(ctx, latest.ID)
		if err != nil {
			return nil, fmt.Errorf("get prior insights prompt: %w", err)
		}
		baseline = prompt.RecoverInsightsBaseline(job.Prompt)
	}
	if job.Status != storage.JobStatusDone || job.FinishedAt == nil || !job.FinishedAt.Equal(*review.Job.FinishedAt) {
		return nil, nil
	}
	if baseline != nil {
		baselineAsOf := baseline.AsOf
		if !strings.ContainsAny(job.FinishedAtRaw, ".,") {
			// Older completions stored whole seconds, losing the fractional part.
			baselineAsOf = baselineAsOf.Truncate(time.Second)
		}
		if baseline.RepoPath != repoPath || baseline.Branch != branch || baselineAsOf.After(*job.FinishedAt) {
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
	// The candidate query identifies code reviews, including legacy rows
	// whose diff is not loaded. Use the verdict of the joined review version.
	verdict := review.Verdict()
	if j == nil || j.Status != storage.JobStatusDone || j.FinishedAt == nil || j.FinishedAt.Before(since) || j.FinishedAt.After(asOf) ||
		j.Error != "" || review.Output == "" || verdict != storage.VerdictFail {
		return prompt.InsightsReview{}, false
	}
	entry := prompt.InsightsReviewFromJob(*j, review.Output, nil, review.Closed)
	entry.Verdict = string(verdict)
	if review.UUID != nil {
		entry.ReviewUUID = review.UUID.String()
	}
	return entry, true
}
