package backfill

import (
	"fmt"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

const (
	ResultUpdated = "updated"
	ResultSkipped = "skipped"
	ResultFailed  = "failed"
)

type SessionUsage struct {
	SessionID string
	Usage     *tokens.Usage
}

type TokenResult struct {
	SessionID string `json:"session_id"`
	JobID     int64  `json:"job_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Summary   string `json:"summary,omitempty"`
}

type TokenSummary struct {
	Total   int           `json:"total"`
	Updated int           `json:"updated"`
	Skipped int           `json:"skipped"`
	Failed  int           `json:"failed"`
	Results []TokenResult `json:"results"`
}

// TokenCandidates filters jobs to those eligible for token backfill:
// completed, has a session ID, missing cost data, and the session was
// not reused by another started job.
func TokenCandidates(jobs []storage.ReviewJob) []storage.ReviewJob {
	sessionCount := make(map[string]int)
	for _, job := range jobs {
		if job.SessionID != "" && job.StartedAt != nil {
			sessionCount[job.SessionID]++
		}
	}

	var out []storage.ReviewJob
	for _, job := range jobs {
		if !job.HasViewableOutput() {
			continue
		}
		if !NeedsTokenCostBackfill(job.TokenUsage) {
			continue
		}
		if job.SessionID == "" {
			continue
		}
		if sessionCount[job.SessionID] > 1 {
			continue
		}
		out = append(out, job)
	}
	return out
}

// LogTokenCandidates filters jobs whose per-job logs may contain recoverable
// token usage. Unlike TokenCandidates, this does not require a session ID or a
// unique session because the usage event came from the individual job log.
func LogTokenCandidates(jobs []storage.ReviewJob) []storage.ReviewJob {
	var out []storage.ReviewJob
	for _, job := range jobs {
		if !job.HasViewableOutput() {
			continue
		}
		if !NeedsTokenUsageBackfill(job.TokenUsage) {
			continue
		}
		out = append(out, job)
	}
	return out
}

func MergeTokenUsage(existingJSON string, fetched *tokens.Usage) *tokens.Usage {
	if fetched == nil {
		return tokens.ParseJSON(existingJSON)
	}
	merged := *fetched
	existing := tokens.ParseJSON(existingJSON)
	if existing == nil {
		return &merged
	}

	if merged.OutputTokens == 0 && merged.PeakContextTokens == 0 {
		merged.OutputTokens = existing.OutputTokens
		merged.PeakContextTokens = existing.PeakContextTokens
	}
	if merged.InputTokens == 0 {
		merged.InputTokens = existing.InputTokens
	}
	if merged.CachedInputTokens == 0 {
		merged.CachedInputTokens = existing.CachedInputTokens
	}
	if merged.UsageSource == "" {
		merged.UsageSource = existing.UsageSource
	}
	if merged.ThreadID == "" {
		merged.ThreadID = existing.ThreadID
	}
	if merged.EventOffset == 0 {
		merged.EventOffset = existing.EventOffset
	}
	if !merged.HasCost && existing.HasCost {
		merged.CostUSD = existing.CostUSD
		merged.HasCost = true
	}
	return &merged
}

func NeedsTokenCostBackfill(tokenUsage string) bool {
	usage := tokens.ParseJSON(tokenUsage)
	return usage == nil || !usage.HasCost
}

func NeedsTokenUsageBackfill(tokenUsage string) bool {
	usage := tokens.ParseJSON(tokenUsage)
	if usage == nil {
		return true
	}
	hasTokenCounts := usage.InputTokens != 0 ||
		usage.CachedInputTokens != 0 ||
		usage.OutputTokens != 0 ||
		usage.PeakContextTokens != 0
	return !hasTokenCounts || !usage.HasCost
}

func ApplyTokenUsage(
	db *storage.DB, sessions []SessionUsage, dryRun bool,
) (TokenSummary, error) {
	jobs, err := db.ListJobs("", "", 0, 0)
	if err != nil {
		return TokenSummary{}, fmt.Errorf("list jobs: %w", err)
	}

	candidates := make(map[string]storage.ReviewJob)
	for _, job := range TokenCandidates(jobs) {
		candidates[job.SessionID] = job
	}

	summary := TokenSummary{
		Total:   len(sessions),
		Results: make([]TokenResult, 0, len(sessions)),
	}
	seen := make(map[string]bool)
	for _, session := range sessions {
		result := TokenResult{SessionID: session.SessionID}
		switch {
		case session.SessionID == "":
			result.Status = ResultSkipped
			result.Reason = "missing session ID"
			summary.Skipped++
		case seen[session.SessionID]:
			result.Status = ResultSkipped
			result.Reason = "duplicate session"
			summary.Skipped++
		case session.Usage == nil:
			seen[session.SessionID] = true
			result.Status = ResultSkipped
			result.Reason = "no usage"
			summary.Skipped++
		default:
			seen[session.SessionID] = true
			job, ok := candidates[session.SessionID]
			if !ok {
				result.Status = ResultSkipped
				result.Reason = "no eligible job"
				summary.Skipped++
				summary.Results = append(summary.Results, result)
				continue
			}

			merged := MergeTokenUsage(job.TokenUsage, session.Usage)
			result.JobID = job.ID
			result.Agent = job.Agent
			result.Summary = merged.FormatSummary()
			if !dryRun {
				if err := db.SaveJobTokenUsage(job.ID, job.SessionID, tokens.ToJSON(merged)); err != nil {
					result.Status = ResultFailed
					result.Reason = err.Error()
					summary.Failed++
					summary.Results = append(summary.Results, result)
					continue
				}
			}
			result.Status = ResultUpdated
			summary.Updated++
		}
		summary.Results = append(summary.Results, result)
	}
	return summary, nil
}
