package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"time"

	"go.kenn.io/roborev/internal/backfill"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

const (
	tokenCostRetryBufferSize   = 1024
	tokenCostScanInterval      = time.Minute
	tokenCostRetryInterval     = 15 * time.Second
	tokenCostPageSize          = 20
	tokenCostImmediateAttempts = 3
	tokenUsageLogScanInterval  = 15 * time.Minute
	tokenUsageLogPageSize      = 1000
	// tokenCostMaxCandidateAge bounds both periodic scans: usage indexing lags
	// by minutes, so a week-old miss means the data is gone (deleted session
	// or job log, retired provider) and endlessly rescanning would never
	// resolve it. Older jobs remain reachable via `roborev backfill-tokens`,
	// which scans without an age bound.
	tokenCostMaxCandidateAge = 7 * 24 * time.Hour
	// tokenCostNoUsageSettle is how long after a job finishes a "session has
	// no usage" answer is final. AgentsView can store a Codex session before
	// it parses the session's token_count line, and its fallback reconcile
	// runs every 15 minutes (periodicSyncInterval in cmd/agentsview/main.go,
	// AgentsView v0.47.0).
	tokenCostNoUsageSettle = 15 * time.Minute
)

type tokenCostRetryState struct {
	attempts       int
	ticksRemaining int
}

// tokenCostMissDelay doubles the wait from the scan interval with each miss.
// It never exceeds the candidate age window, after which the job leaves the
// scan anyway.
func (wp *WorkerPool) tokenCostMissDelay(count int) time.Duration {
	limit := wp.tokenCostMaxCandidateAge
	delay := wp.tokenCostScanInterval
	for i := 1; i < count && delay < limit; i++ {
		delay *= 2
	}
	return min(delay, limit)
}

func (wp *WorkerPool) queueTokenCostRetry(jobID int64) {
	if jobID <= 0 {
		return
	}
	select {
	case wp.tokenCostRetryCh <- jobID:
	default:
		// The persistent scan is the durable fallback when the short-retry
		// buffer is full.
	}
}

func (wp *WorkerPool) runTokenCostReconciler() {
	defer wp.wg.Done()

	wp.recoverTokenUsageLogs(wp.stopCtx)
	cursor := wp.reconcileTokenCostPage(wp.stopCtx, 0)
	scanTicker := time.NewTicker(wp.tokenCostScanInterval)
	defer scanTicker.Stop()
	logScanTicker := time.NewTicker(wp.tokenUsageLogScanInterval)
	defer logScanTicker.Stop()
	retryTicker := time.NewTicker(wp.tokenCostRetryInterval)
	defer retryTicker.Stop()
	pending := make(map[int64]tokenCostRetryState)

	for {
		select {
		case <-wp.stopCtx.Done():
			return
		case jobID := <-wp.tokenCostRetryCh:
			wp.addTokenCostRetry(pending, jobID)
		case <-retryTicker.C:
			wp.processTokenCostRetries(wp.stopCtx, pending)
		case <-scanTicker.C:
			cursor = wp.reconcileTokenCostPage(wp.stopCtx, cursor)
		case <-logScanTicker.C:
			wp.recoverTokenUsageLogs(wp.stopCtx)
		}
	}
}

func (wp *WorkerPool) recoverTokenUsageLogs(ctx context.Context) {
	var cursor int64
	for {
		if ctx.Err() != nil {
			return
		}
		candidates, err := wp.db.ListTokenUsageLogCandidates(
			cursor, wp.tokenUsageLogPageSize,
			time.Now().Add(-wp.tokenCostMaxCandidateAge),
		)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("token cost reconciliation: recover job logs: %v", err)
			}
			return
		}
		if len(candidates) == 0 {
			return
		}
		for _, candidate := range candidates {
			wp.recoverTokenUsageLog(candidate)
			cursor = candidate.JobID
		}
	}
}

func (wp *WorkerPool) recoverTokenUsageLog(candidate storage.TokenUsageLogCandidate) {
	current, err := JobLogIsCurrentAttempt(candidate.JobID, &candidate.StartedAt)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("token cost reconciliation: job %d log: %v", candidate.JobID, err)
		return
	}
	if !current {
		return
	}
	existing := tokens.ParseJSON(candidate.TokenUsage)
	logUsage, err := tokens.ParseCodexUsageFile(JobLogPath(candidate.JobID))
	if err != nil {
		log.Printf("token cost reconciliation: job %d log: %v", candidate.JobID, err)
		return
	}

	sessionID := ""
	if logUsage != nil {
		sessionID = logUsage.ThreadID
	}
	if sessionID == "" && existing != nil {
		sessionID = existing.ThreadID
	}
	if sessionID == "" {
		return
	}
	usage := logUsage
	if usage == nil {
		usage = existing
	}
	_, updated, err := backfill.StoreMergedTokenUsage(
		wp.db,
		backfill.CapturedUsage{
			JobID:             candidate.JobID,
			SessionID:         sessionID,
			ExistingJSON:      candidate.TokenUsage,
			ExpectedStartedAt: candidate.StartedAtRaw,
		},
		usage,
		false,
	)
	if err != nil {
		log.Printf("token cost reconciliation: job %d log save: %v", candidate.JobID, err)
		return
	}
	if updated && wp.budgetRouter != nil {
		wp.budgetRouter.Invalidate()
	}
}

func (wp *WorkerPool) addTokenCostRetry(
	pending map[int64]tokenCostRetryState, jobID int64,
) bool {
	if _, exists := pending[jobID]; exists {
		return true
	}
	if len(pending) >= wp.tokenCostPendingLimit {
		return false
	}
	pending[jobID] = tokenCostRetryState{ticksRemaining: 1}
	return true
}

func (wp *WorkerPool) processTokenCostRetries(
	ctx context.Context, pending map[int64]tokenCostRetryState,
) {
	jobIDs := make([]int64, 0, len(pending))
	for jobID, state := range pending {
		if state.ticksRemaining > 0 {
			state.ticksRemaining--
			pending[jobID] = state
		}
		if state.ticksRemaining == 0 {
			jobIDs = append(jobIDs, jobID)
		}
	}
	slices.Sort(jobIDs)
	if len(jobIDs) > wp.tokenCostPageSize {
		jobIDs = jobIDs[:wp.tokenCostPageSize]
	}

	providerUnavailableLogged := false
	for _, jobID := range jobIDs {
		resolved, err := wp.reconcileTokenCostJob(ctx, jobID)
		if resolved {
			delete(pending, jobID)
			continue
		}
		if err != nil && !errors.Is(err, tokens.ErrSessionWithoutUsage) {
			if errors.Is(err, tokens.ErrUsageProviderUnavailable) {
				if !providerUnavailableLogged {
					log.Printf("token cost reconciliation: usage provider unavailable: %v", err)
					providerUnavailableLogged = true
				}
			} else if !errors.Is(err, context.Canceled) {
				log.Printf("token cost reconciliation: job %d: %v", jobID, err)
			}
		}
		state := pending[jobID]
		state.attempts++
		if state.attempts >= wp.tokenCostImmediateAttempts {
			delete(pending, jobID)
			continue
		}
		state.ticksRemaining = 1 << state.attempts
		pending[jobID] = state
	}
}

// reconcileTokenCostPage looks up one page of due candidates. Each lookup can
// start an agentsview process or send an HTTP request, so a job that does not
// resolve is rechecked with exponential backoff, and lookups stop once the
// provider settles that the session has no usage. The state is stored per job
// attempt so a daemon restart keeps it.
func (wp *WorkerPool) reconcileTokenCostPage(
	ctx context.Context, afterJobID int64,
) int64 {
	now := time.Now()
	startedAfter := now.Add(-wp.tokenCostMaxCandidateAge)
	candidates, err := wp.db.ListDueTokenCostCandidates(
		ctx, afterJobID, wp.tokenCostPageSize, startedAfter, now,
	)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("token cost reconciliation: %v", err)
		}
		return afterJobID
	}
	if len(candidates) < wp.tokenCostPageSize {
		// The scan wraps here, so drop state for jobs that left the scan.
		if err := wp.db.PruneTokenCostLookups(ctx, startedAfter); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Printf("token cost reconciliation: %v", err)
		}
	}
	if len(candidates) == 0 {
		return 0
	}

	cursor := afterJobID
	for _, candidate := range candidates {
		resolved, err := wp.reconcileTokenCostCandidate(ctx, candidate.TokenCostCandidate)
		noUsageSessionID := ""
		if noUsage, ok := errors.AsType[*sessionWithoutUsageError](err); ok {
			noUsageSessionID = noUsage.sessionID
		}
		if err != nil && !errors.Is(err, tokens.ErrSessionWithoutUsage) {
			if errors.Is(err, context.Canceled) {
				return cursor
			}
			if errors.Is(err, tokens.ErrUsageProviderUnavailable) {
				log.Printf("token cost reconciliation: usage provider unavailable: %v", err)
				return cursor
			}
			log.Printf("token cost reconciliation: job %d: %v", candidate.JobID, err)
		}
		if !resolved {
			wp.recordTokenCostMiss(ctx, candidate, noUsageSessionID, now)
		}
		cursor = candidate.JobID
	}
	if len(candidates) < wp.tokenCostPageSize {
		return 0
	}
	return cursor
}

// recordTokenCostMiss schedules the next lookup of a candidate that did not
// price. noUsageSessionID names the provider session this lookup found
// without usage, or is empty. Lookups stop when the provider reports the same
// session without usage on two lookups in a row, the later one at least
// tokenCostNoUsageSettle after the job finished, so usage that was still being
// indexed gets a second look. A job with several phase sessions keeps going
// when the empty answer moves from one session to another.
func (wp *WorkerPool) recordTokenCostMiss(
	ctx context.Context, candidate storage.DueTokenCostCandidate, noUsageSessionID string, now time.Time,
) {
	lookup := storage.TokenCostLookup{
		JobID:            candidate.JobID,
		StartedAt:        candidate.StartedAtRaw,
		SessionID:        candidate.SessionID,
		Misses:           candidate.LookupMisses + 1,
		NoUsageSessionID: noUsageSessionID,
	}
	settled := noUsageSessionID != "" &&
		noUsageSessionID == candidate.LastNoUsageSessionID &&
		now.Sub(candidate.FinishedAt) >= tokenCostNoUsageSettle
	if !settled {
		lookup.NextCheckAt = now.Add(wp.tokenCostMissDelay(lookup.Misses))
	}
	if err := wp.db.SaveTokenCostLookup(ctx, lookup); err != nil &&
		!errors.Is(err, context.Canceled) {
		log.Printf("token cost reconciliation: job %d: %v", candidate.JobID, err)
	}
}

func (wp *WorkerPool) reconcileTokenCostJob(
	ctx context.Context, jobID int64,
) (bool, error) {
	candidate, err := wp.db.GetTokenCostCandidate(jobID)
	if err != nil {
		return false, err
	}
	if candidate == nil {
		return true, nil
	}
	return wp.reconcileTokenCostCandidate(ctx, *candidate)
}

func (wp *WorkerPool) reconcileTokenCostCandidate(
	ctx context.Context, candidate storage.TokenCostCandidate,
) (bool, error) {
	sessionIDs, expectedSessions := tokenUsageProviderSessions(candidate)
	fetchedSessions := make([]*tokens.Usage, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		// ErrSessionWithoutUsage passes through, naming the session, so the
		// caller can stop asking about a session that will never have a price.
		fetched, err := wp.fetchTokenUsage(ctx, sessionID)
		if errors.Is(err, tokens.ErrSessionWithoutUsage) {
			return false, &sessionWithoutUsageError{sessionID: sessionID, err: err}
		}
		if err != nil {
			return false, err
		}
		if backfill.NeedsTokenCostBackfill(tokens.ToJSON(fetched)) {
			return false, nil
		}
		fetchedSessions = append(fetchedSessions, fetched)
	}
	fetched := aggregateTokenUsages(fetchedSessions)
	if fetched == nil {
		return false, nil
	}
	setReconciledSessions(fetched, candidate.SessionID, sessionIDs, expectedSessions)
	merged := backfill.MergeTokenUsage(candidate.TokenUsage, fetched)
	if backfill.NeedsTokenCostBackfill(tokens.ToJSON(merged)) {
		return false, nil
	}
	_, updated, err := backfill.StoreMergedTokenUsage(
		wp.db,
		backfill.CapturedUsage{
			JobID:             candidate.JobID,
			SessionID:         candidate.SessionID,
			ExistingJSON:      candidate.TokenUsage,
			ExpectedStartedAt: candidate.StartedAtRaw,
		},
		fetched,
		true,
	)
	if err != nil {
		return false, err
	}
	if updated && wp.budgetRouter != nil {
		wp.budgetRouter.Invalidate()
	}
	return updated, nil
}

// sessionWithoutUsageError names the provider session that the usage provider
// reported without usage. It wraps tokens.ErrSessionWithoutUsage.
type sessionWithoutUsageError struct {
	sessionID string
	err       error
}

func (e *sessionWithoutUsageError) Error() string {
	return fmt.Sprintf("provider session %s: %v", e.sessionID, e.err)
}

func (e *sessionWithoutUsageError) Unwrap() error { return e.err }

// setReconciledSessions records the sessions reconciliation priced. When some
// expected sessions have no ID, the stored row already counts every phase's
// tokens, so only the fetched dollars are kept: MergeTokenUsage then refills
// the stored counts and keeps the higher stored session count, so the row
// keeps its known dollars while its coverage stays partial.
func setReconciledSessions(fetched *tokens.Usage, sessionID string, sessionIDs []string, expectedSessions int) {
	if expectedSessions > len(sessionIDs) {
		*fetched = tokens.Usage{CostUSD: fetched.CostUSD, HasCost: fetched.HasCost}
	}
	setTokenUsageSessions(fetched, sessionID, sessionIDs, len(sessionIDs))
}

func tokenUsageProviderSessions(candidate storage.TokenCostCandidate) ([]string, int) {
	return backfill.TokenUsageProviderSessions(candidate)
}
