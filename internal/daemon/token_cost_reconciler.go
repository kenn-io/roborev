package daemon

import (
	"context"
	"errors"
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
)

type tokenCostRetryState struct {
	attempts       int
	ticksRemaining int
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
	_, _, err = backfill.StoreMergedTokenUsage(
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
		if err != nil {
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

func (wp *WorkerPool) reconcileTokenCostPage(
	ctx context.Context, afterJobID int64,
) int64 {
	candidates, err := wp.db.ListTokenCostCandidates(
		afterJobID, wp.tokenCostPageSize,
		time.Now().Add(-wp.tokenCostMaxCandidateAge),
	)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("token cost reconciliation: %v", err)
		}
		return afterJobID
	}
	if len(candidates) == 0 {
		return 0
	}

	cursor := afterJobID
	for _, candidate := range candidates {
		_, err := wp.reconcileTokenCostCandidate(ctx, candidate)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return cursor
			}
			if errors.Is(err, tokens.ErrUsageProviderUnavailable) {
				log.Printf("token cost reconciliation: usage provider unavailable: %v", err)
				return cursor
			}
			log.Printf("token cost reconciliation: job %d: %v", candidate.JobID, err)
		}
		cursor = candidate.JobID
	}
	if len(candidates) < wp.tokenCostPageSize {
		return 0
	}
	return cursor
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
	fetched, err := wp.fetchTokenUsage(ctx, candidate.SessionID)
	if err != nil {
		return false, err
	}
	if backfill.NeedsTokenCostBackfill(tokens.ToJSON(fetched)) {
		return false, nil
	}
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
	return updated, nil
}
