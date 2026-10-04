package backfill

import (
	"fmt"
	"time"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

const (
	ResultUpdated = "updated"
	ResultSkipped = "skipped"
	ResultFailed  = "failed"
	storeAttempts = 3
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

// LogTokenCandidates filters started jobs whose per-job logs may contain
// recoverable token usage. This does not require a session ID or a unique
// session because the usage event came from the individual job log.
func LogTokenCandidates(jobs []storage.ReviewJob) []storage.ReviewJob {
	var out []storage.ReviewJob
	for _, job := range jobs {
		if !hasTerminalStatus(job.Status) || job.StartedAt == nil {
			continue
		}
		if !NeedsTokenUsageBackfill(job.TokenUsage) {
			continue
		}
		out = append(out, job)
	}
	return out
}

func hasTerminalStatus(status storage.JobStatus) bool {
	switch status {
	case storage.JobStatusDone,
		storage.JobStatusApplied,
		storage.JobStatusRebased,
		storage.JobStatusFailed,
		storage.JobStatusCanceled,
		storage.JobStatusSkipped:
		return true
	default:
		return false
	}
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
	if merged.CacheCreationTokens == 0 {
		merged.CacheCreationTokens = existing.CacheCreationTokens
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
	merged.ProviderSessionIDs = mergeProviderSessionIDs(
		existing.ProviderSessionIDs, merged.ProviderSessionIDs,
	)
	if merged.ExpectedProviderSessions < existing.ExpectedProviderSessions {
		merged.ExpectedProviderSessions = existing.ExpectedProviderSessions
	}
	if merged.ExpectedProviderSessions < len(merged.ProviderSessionIDs) {
		merged.ExpectedProviderSessions = len(merged.ProviderSessionIDs)
	}
	// Keep whichever side actually carries dollars rather than the freshest one:
	// a re-fetch can come back unpriced (agentsview flagging has_cost with no
	// amount), and letting that overwrite a real recorded figure would lose
	// spend that was already measured.
	//
	// Gating on hasRecordedCost means a stored flag with no amount is never
	// carried forward. Such a row is exactly what this repair removes, and
	// resurrecting the flag would keep it in the priced numerator at $0. A
	// freshly fetched $0 still survives, since that is a real free run rather
	// than an amount that went missing.
	if hasRecordedCost(existingJSON) && (!merged.HasCost || merged.CostUSD == 0) {
		merged.CostUSD = existing.CostUSD
		merged.HasCost = true
	}
	return &merged
}

func mergeProviderSessionIDs(existing, fetched []string) []string {
	if len(existing)+len(fetched) == 0 {
		return nil
	}
	merged := make([]string, 0, len(existing)+len(fetched))
	seen := make(map[string]struct{}, len(existing)+len(fetched))
	for _, sessionIDs := range [][]string{existing, fetched} {
		for _, sessionID := range sessionIDs {
			if sessionID == "" {
				continue
			}
			if _, ok := seen[sessionID]; ok {
				continue
			}
			seen[sessionID] = struct{}{}
			merged = append(merged, sessionID)
		}
	}
	return merged
}

// mergeMissingTokenUsage treats the persisted row as authoritative after a
// compare-and-swap conflict. The provider snapshot may have been fetched
// before normal capture updated the row, so it may only fill fields that are
// still absent; it must never replace newer counts or pricing.
func mergeMissingTokenUsage(existingJSON string, fetched *tokens.Usage) *tokens.Usage {
	existing := tokens.ParseJSON(existingJSON)
	if existing == nil {
		return MergeTokenUsage(existingJSON, fetched)
	}
	merged := *existing
	if fetched == nil {
		return &merged
	}

	if merged.OutputTokens == 0 && merged.PeakContextTokens == 0 {
		merged.OutputTokens = fetched.OutputTokens
		merged.PeakContextTokens = fetched.PeakContextTokens
	}
	if merged.InputTokens == 0 {
		merged.InputTokens = fetched.InputTokens
	}
	if merged.CachedInputTokens == 0 {
		merged.CachedInputTokens = fetched.CachedInputTokens
	}
	if merged.CacheCreationTokens == 0 {
		merged.CacheCreationTokens = fetched.CacheCreationTokens
	}
	if merged.UsageSource == "" {
		merged.UsageSource = fetched.UsageSource
	}
	if merged.ThreadID == "" {
		merged.ThreadID = fetched.ThreadID
	}
	if merged.EventOffset == 0 {
		merged.EventOffset = fetched.EventOffset
	}
	if len(merged.ProviderSessionIDs) == 0 {
		merged.ProviderSessionIDs = append([]string(nil), fetched.ProviderSessionIDs...)
	}
	if merged.ExpectedProviderSessions == 0 {
		merged.ExpectedProviderSessions = fetched.ExpectedProviderSessions
	}
	if !merged.HasCost && fetched.HasCost {
		merged.CostUSD = fetched.CostUSD
		merged.HasCost = true
	}
	return &merged
}

// hasRecordedCost reports whether a row carries an actual dollar figure, not
// just the has_cost flag. The two came apart when agentsview v0.39.0 moved cost
// into a microdollar envelope roborev could not yet read: the flag was stored,
// the amount was lost, and the row silently priced itself at $0.
//
// Presence of the cost_usd key is what separates the two, which is why Usage
// serializes it unconditionally. A row recording an explicit 0 is a real free
// run and is left alone; a row flagged priced with no amount at all is the
// drifted shape and needs re-fetching.
func hasRecordedCost(tokenUsage string) bool {
	usage := tokens.ParseJSON(tokenUsage)
	return usage != nil && usage.HasCost
}

func NeedsTokenCostBackfill(tokenUsage string) bool {
	return !hasRecordedCost(tokenUsage)
}

func NeedsTokenUsageBackfill(tokenUsage string) bool {
	usage := tokens.ParseJSON(tokenUsage)
	if usage == nil {
		return true
	}
	hasTokenCounts := usage.InputTokens != 0 ||
		usage.CachedInputTokens != 0 ||
		usage.CacheCreationTokens != 0 ||
		usage.OutputTokens != 0 ||
		usage.PeakContextTokens != 0
	return !hasTokenCounts || !hasRecordedCost(tokenUsage)
}

// CapturedUsage identifies the job attempt a recovered usage payload belongs
// to. ExistingJSON is the usage snapshot loaded before the recovery attempt;
// a non-empty ExpectedStartedAt pins writes to that attempt.
type CapturedUsage struct {
	JobID             int64
	SessionID         string
	ExistingJSON      string
	ExpectedStartedAt string
}

// StoreMergedTokenUsage atomically merges recovered usage into a terminal job.
// If normal capture updates the row during a provider lookup, this reloads the
// latest usage and retries rather than overwriting newer token counts. Callers
// can require storage to reject a provider session that another started job
// began using in the meantime; per-job log usage is safe without that guard.
func StoreMergedTokenUsage(
	db *storage.DB,
	captured CapturedUsage,
	fetched *tokens.Usage,
	requireUniqueSession bool,
) (*tokens.Usage, bool, error) {
	var merged *tokens.Usage
	existingJSON := captured.ExistingJSON
	afterConflict := false
	for range storeAttempts {
		if afterConflict {
			merged = mergeMissingTokenUsage(existingJSON, fetched)
		} else {
			merged = MergeTokenUsage(existingJSON, fetched)
		}
		if merged == nil {
			return nil, false, nil
		}
		updated, err := db.BackfillJobTokenUsageIfCurrent(storage.TokenUsageWrite{
			JobID:                captured.JobID,
			SessionID:            captured.SessionID,
			ExpectedTokenUsage:   existingJSON,
			TokenUsageJSON:       tokens.ToJSON(merged),
			ExpectedStartedAt:    captured.ExpectedStartedAt,
			RequireUniqueSession: requireUniqueSession,
		})
		if err != nil || updated {
			return merged, updated, err
		}

		current, err := db.GetJobByID(captured.JobID)
		if err != nil {
			return merged, false, err
		}
		if current.TokenUsage == existingJSON ||
			(current.SessionID != "" && current.SessionID != captured.SessionID) {
			return merged, false, nil
		}
		if hasRecordedCost(current.TokenUsage) {
			return tokens.ParseJSON(current.TokenUsage), false, nil
		}
		existingJSON = current.TokenUsage
		afterConflict = true
	}
	return merged, false, nil
}

// StoreCapturedTokenUsage persists per-job log usage before provider usage.
// Log counts belong to one job even when a provider session was reused;
// provider totals are cumulative and therefore require a unique session. The
// expected start time keeps both writes bound to the selected attempt.
func StoreCapturedTokenUsage(
	db *storage.DB,
	captured CapturedUsage,
	logUsage, providerUsage *tokens.Usage,
) (*tokens.Usage, bool, error) {
	var stored *tokens.Usage
	anySaved := false
	for _, source := range []struct {
		usage                *tokens.Usage
		requireUniqueSession bool
	}{
		{usage: logUsage},
		{usage: providerUsage, requireUniqueSession: true},
	} {
		if source.usage == nil {
			continue
		}
		merged, saved, err := StoreMergedTokenUsage(
			db, captured, source.usage, source.requireUniqueSession,
		)
		if err != nil {
			return stored, anySaved, err
		}
		if !saved {
			continue
		}
		stored = merged
		anySaved = true
		captured.ExistingJSON = tokens.ToJSON(merged)
	}
	return stored, anySaved, nil
}

// TokenUsageProviderSessions returns the distinct provider sessions represented
// by a candidate and the number of phases expected in its aggregate.
func TokenUsageProviderSessions(candidate storage.TokenCostCandidate) ([]string, int) {
	var sessionIDs []string
	expectedSessions := 0
	if usage := tokens.ParseJSON(candidate.TokenUsage); usage != nil {
		sessionIDs = append(sessionIDs, usage.ProviderSessionIDs...)
		expectedSessions = usage.ExpectedProviderSessions
	}
	sessionIDs = append(sessionIDs, candidate.SessionID)
	seen := make(map[string]bool, len(sessionIDs))
	unique := sessionIDs[:0]
	for _, sessionID := range sessionIDs {
		if sessionID == "" || seen[sessionID] {
			continue
		}
		seen[sessionID] = true
		unique = append(unique, sessionID)
	}
	if expectedSessions < len(unique) {
		expectedSessions = len(unique)
	}
	return unique, expectedSessions
}

// AggregateTokenUsages sums usage values while preserving the metadata carried
// by the last non-empty phase. HasCost is true only when every supplied phase
// has a recorded cost. Token counts are kept only when every phase has count
// data, so a cost-only phase cannot make the aggregate look like a complete
// count total.
func AggregateTokenUsages(usages []*tokens.Usage) *tokens.Usage {
	aggregate := &tokens.Usage{}
	allPriced := len(usages) > 0
	allHaveTokenCounts := len(usages) > 0
	hasUsage := false
	for _, usage := range usages {
		if usage == nil {
			allPriced = false
			allHaveTokenCounts = false
			continue
		}
		if !usage.HasCost {
			allPriced = false
		} else {
			aggregate.CostUSD += usage.CostUSD
		}
		if !hasTokenCounts(*usage) {
			allHaveTokenCounts = false
		}
		aggregate.InputTokens += usage.InputTokens
		aggregate.CachedInputTokens += usage.CachedInputTokens
		aggregate.CacheCreationTokens += usage.CacheCreationTokens
		aggregate.OutputTokens += usage.OutputTokens
		if usage.PeakContextTokens > aggregate.PeakContextTokens {
			aggregate.PeakContextTokens = usage.PeakContextTokens
		}
		if usage.UsageSource != "" {
			aggregate.UsageSource = usage.UsageSource
		}
		if usage.ThreadID != "" {
			aggregate.ThreadID = usage.ThreadID
		}
		aggregate.EventOffset = usage.EventOffset
		hasUsage = hasUsage || usage.HasUsageData()
	}
	if !hasUsage {
		return nil
	}
	if !allHaveTokenCounts {
		aggregate.InputTokens = 0
		aggregate.CachedInputTokens = 0
		aggregate.CacheCreationTokens = 0
		aggregate.OutputTokens = 0
		aggregate.PeakContextTokens = 0
	}
	aggregate.HasCost = allPriced
	return aggregate
}

func hasTokenCounts(usage tokens.Usage) bool {
	return usage.InputTokens != 0 ||
		usage.CachedInputTokens != 0 ||
		usage.CacheCreationTokens != 0 ||
		usage.OutputTokens != 0 ||
		usage.PeakContextTokens != 0
}

func ApplyTokenUsage(
	db *storage.DB, sessions []SessionUsage, dryRun bool,
) (TokenSummary, error) {
	candidates := make(map[int64]storage.TokenCostCandidate)
	candidatesBySession := make(map[string]int64)
	ambiguousSessions := make(map[string]bool)
	var cursor int64
	for {
		page, err := db.ListTokenCostCandidates(cursor, 1000, time.Time{})
		if err != nil {
			return TokenSummary{}, fmt.Errorf("list token cost candidates: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, candidate := range page {
			candidates[candidate.JobID] = candidate
			sessionIDs, _ := TokenUsageProviderSessions(candidate)
			for _, sessionID := range sessionIDs {
				if previous, ok := candidatesBySession[sessionID]; ok && previous != candidate.JobID {
					ambiguousSessions[sessionID] = true
					continue
				}
				candidatesBySession[sessionID] = candidate.JobID
			}
		}
		cursor = page[len(page)-1].JobID
	}

	summary := TokenSummary{
		Total:   len(sessions),
		Results: make([]TokenResult, len(sessions)),
	}
	type usageImportGroup struct {
		candidate storage.TokenCostCandidate
		usageByID map[string]*tokens.Usage
		indexes   []int
	}
	groups := make(map[int64]*usageImportGroup)
	seen := make(map[string]bool)
	for index, session := range sessions {
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
			jobID, ok := candidatesBySession[session.SessionID]
			if !ok {
				result.Status = ResultSkipped
				result.Reason = "no eligible job"
				summary.Skipped++
			} else if ambiguousSessions[session.SessionID] {
				result.Status = ResultSkipped
				result.Reason = "ambiguous eligible job"
				summary.Skipped++
			} else {
				job := candidates[jobID]
				result.JobID = job.JobID
				result.Agent = job.Agent
				group := groups[jobID]
				if group == nil {
					group = &usageImportGroup{
						candidate: job,
						usageByID: make(map[string]*tokens.Usage),
					}
					groups[jobID] = group
				}
				group.usageByID[session.SessionID] = session.Usage
				group.indexes = append(group.indexes, index)
			}
		}
		summary.Results[index] = result
	}

	for _, group := range groups {
		sessionIDs, expected := TokenUsageProviderSessions(group.candidate)
		if expected > len(sessionIDs) || len(group.usageByID) != expected {
			markImportGroup(summary.Results, group.indexes, ResultSkipped,
				"incomplete provider session set", "")
			summary.Skipped += len(group.indexes)
			continue
		}
		phaseUsages := make([]*tokens.Usage, 0, expected)
		complete := true
		for _, sessionID := range sessionIDs {
			usage, ok := group.usageByID[sessionID]
			if !ok {
				complete = false
				break
			}
			phaseUsages = append(phaseUsages, usage)
		}
		if !complete {
			markImportGroup(summary.Results, group.indexes, ResultSkipped,
				"incomplete provider session set", "")
			summary.Skipped += len(group.indexes)
			continue
		}
		aggregate := AggregateTokenUsages(phaseUsages)
		if aggregate == nil {
			markImportGroup(summary.Results, group.indexes, ResultSkipped,
				"no usage", "")
			summary.Skipped += len(group.indexes)
			continue
		}
		aggregate.ThreadID = group.candidate.SessionID
		aggregate.ProviderSessionIDs = sessionIDs
		aggregate.ExpectedProviderSessions = expected
		merged := MergeTokenUsage(group.candidate.TokenUsage, aggregate)
		if !dryRun {
			stored, updated, err := StoreMergedTokenUsage(
				db, CapturedUsage{
					JobID:             group.candidate.JobID,
					SessionID:         group.candidate.SessionID,
					ExistingJSON:      group.candidate.TokenUsage,
					ExpectedStartedAt: group.candidate.StartedAtRaw,
				}, aggregate, true,
			)
			if err != nil {
				markImportGroup(summary.Results, group.indexes, ResultFailed, err.Error(), "")
				summary.Failed += len(group.indexes)
				continue
			}
			if !updated {
				markImportGroup(summary.Results, group.indexes, ResultSkipped,
					"no longer eligible", "")
				summary.Skipped += len(group.indexes)
				continue
			}
			merged = stored
		}
		markImportGroup(summary.Results, group.indexes, ResultUpdated, "", merged.FormatSummary())
		summary.Updated += len(group.indexes)
	}
	return summary, nil
}

func markImportGroup(results []TokenResult, indexes []int, status, reason, summary string) {
	for _, index := range indexes {
		results[index].Status = status
		results[index].Reason = reason
		results[index].Summary = summary
	}
}
