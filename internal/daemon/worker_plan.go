package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/backfill"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

// prepareFixPlan uses a separate session and worktree, retaining only text.
func (wp *WorkerPool) prepareFixPlan(ctx context.Context, planningTimeout time.Duration, workerID string, a agent.Agent, job *storage.ReviewJob, repoPath, planningPrompt, implementationPrompt string, builder *prompt.Builder, configRepoPath string, output io.Writer, onInvoked func()) (string, string, error) {
	planCtx := ctx
	if planningTimeout > 0 {
		var cancel context.CancelFunc
		planCtx, cancel = context.WithTimeout(ctx, planningTimeout)
		defer cancel()
	}
	plan, err := agent.RunPlan(planCtx, a, repoPath, "HEAD", planningPrompt, output, func(path, text string) (string, func(), error) {
		prepared, err := builder.Prepare(text, prompt.SnapshotTarget{RepoPath: path, ConfigRepoPath: configRepoPath})
		if err == nil {
			wp.markAgentInvokedWithCommandLine(workerID, job, agent.CommandLineForPlanning(a))
			if onInvoked != nil {
				onInvoked()
			}
		}
		return prepared.Prompt, prepared.Cleanup, err
	})
	if err != nil {
		return "", "", err
	}
	if err := planCtx.Err(); err != nil {
		return "", "", err
	}
	if job.ParentJobID != nil {
		if _, err := wp.db.AddCommentToJob(*job.ParentJobID, "roborev-plan", "## Plan\n\n"+plan); err != nil {
			return "", "", fmt.Errorf("store fix plan: %w", err)
		}
	}
	return plan, prompt.WithFixPlan(implementationPrompt, plan), nil
}

func (wp *WorkerPool) capturePlanTokenUsage(
	ctx context.Context,
	workerID string,
	job *storage.ReviewJob,
	phases []planTokenPhase,
) {
	snapshot := buildPlanTokenUsageSnapshot(job, phases)
	baseUsage := snapshot.usage
	if baseUsage == nil {
		return
	}
	if snapshot.sessionID == "" {
		return
	}
	if _, err := wp.storePlanTokenUsage(workerID, job, snapshot.sessionID, baseUsage, !snapshot.wasResumed); err != nil {
		log.Printf("[%s] Warning: save initial plan token usage for job %d: %v",
			workerID, job.ID, err)
		return
	}

	phaseUsages := make([]*tokens.Usage, 0, len(phases))
	for i, phase := range phases {
		if !phase.invoked {
			continue
		}
		var providerUsage *tokens.Usage
		if i < len(snapshot.fetchProviderSession) && snapshot.fetchProviderSession[i] {
			sessionID := phase.sessionID
			if sessionID == "" && phase.logUsage != nil {
				sessionID = phase.logUsage.ThreadID
			}
			if phase.implementation && sessionID == "" {
				sessionID = job.SessionID
			}
			fetched, err := wp.fetchFreshSessionUsage(ctx, wp.fetchTokenUsage, sessionID)
			switch {
			case err == nil:
				providerUsage = fetched
			case !errors.Is(err, tokens.ErrUsageProviderUnavailable):
				log.Printf("[%s] Warning: fetch token usage for job %d session %q: %v",
					workerID, job.ID, sessionID, err)
			}
		}
		phaseUsages = append(phaseUsages, backfill.MergeTokenUsage(tokens.ToJSON(phase.logUsage), providerUsage))
	}

	aggregate := aggregateTokenUsages(phaseUsages)
	if aggregate == nil {
		if len(snapshot.providerSessionIDs) >= snapshot.expectedSessions && backfill.NeedsTokenCostBackfill(tokens.ToJSON(baseUsage)) {
			wp.queueTokenCostRetry(job.ID)
		}
		return
	}
	aggregate.ThreadID = snapshot.sessionID
	aggregate.ProviderSessionIDs = snapshot.providerSessionIDs
	aggregate.ExpectedProviderSessions = snapshot.expectedSessions
	if snapshot.expectedSessions > len(snapshot.providerSessionIDs) {
		aggregate.HasCost = false
	}
	_, err := wp.storePlanTokenUsage(workerID, job, snapshot.sessionID, aggregate, !snapshot.wasResumed)
	if err != nil {
		log.Printf("[%s] Warning: save plan token usage for job %d: %v",
			workerID, job.ID, err)
		if len(snapshot.providerSessionIDs) >= snapshot.expectedSessions {
			wp.queueTokenCostRetry(job.ID)
		}
		return
	}
	if len(snapshot.providerSessionIDs) >= snapshot.expectedSessions && backfill.NeedsTokenCostBackfill(tokens.ToJSON(aggregate)) {
		wp.queueTokenCostRetry(job.ID)
	}
}

func (wp *WorkerPool) capturePlanTokenUsageForTerminalAttempt(
	ctx context.Context,
	workerID string,
	job *storage.ReviewJob,
	phases []planTokenPhase,
) {
	current, err := wp.db.GetJobByID(job.ID)
	if err != nil {
		log.Printf("[%s] Warning: reload job %d before terminal plan usage capture: %v",
			workerID, job.ID, err)
		return
	}
	if current.StartedAtRaw != job.StartedAtRaw {
		return
	}
	switch current.Status {
	case storage.JobStatusDone,
		storage.JobStatusApplied,
		storage.JobStatusRebased,
		storage.JobStatusFailed,
		storage.JobStatusCanceled,
		storage.JobStatusSkipped:
	default:
		return
	}
	wp.capturePlanTokenUsage(
		ctx, workerID, job, phases,
	)
}

func (wp *WorkerPool) persistPlanTokenUsageForRunningAttempt(
	workerID string,
	job *storage.ReviewJob,
	phases []planTokenPhase,
	allowAnchorFallback bool,
) {
	snapshot := buildPlanTokenUsageSnapshot(job, phases)
	if snapshot.usage == nil {
		return
	}
	if allowAnchorFallback && snapshot.sessionID != "" {
		current, err := wp.db.GetJobByID(job.ID)
		if err != nil {
			log.Printf("[%s] Warning: reload job %d before saving phase usage: %v",
				workerID, job.ID, err)
			return
		}
		if current.SessionID == "" {
			if err := wp.db.SaveJobSessionID(job.ID, workerID, snapshot.sessionID); err != nil {
				log.Printf("[%s] Warning: save phase usage anchor for job %d: %v",
					workerID, job.ID, err)
				return
			}
		}
	}
	saved, err := wp.db.SaveRunningJobTokenUsage(
		job.ID, workerID, job.StartedAtRaw, tokens.ToJSON(snapshot.usage),
	)
	if err != nil {
		log.Printf("[%s] Warning: save running phase usage for job %d: %v",
			workerID, job.ID, err)
		return
	}
	if saved {
		wp.invalidateBudgetSpend()
	}
}

func (wp *WorkerPool) storePlanTokenUsage(
	workerID string,
	job *storage.ReviewJob,
	sessionID string,
	usage *tokens.Usage,
	requireUniqueSession bool,
) (bool, error) {
	current, err := wp.db.GetJobByID(job.ID)
	if err != nil {
		return false, fmt.Errorf("reload job %d before saving plan token usage: %w", job.ID, err)
	}
	_, updated, err := backfill.StoreMergedTokenUsage(
		wp.db,
		backfill.CapturedUsage{
			JobID:             job.ID,
			SessionID:         sessionID,
			ExistingJSON:      current.TokenUsage,
			ExpectedStartedAt: job.StartedAtRaw,
		},
		usage,
		requireUniqueSession,
	)
	if err != nil {
		return false, err
	}
	if updated {
		wp.invalidateBudgetSpend()
	}
	return updated, nil
}

type planTokenPhase struct {
	invoked        bool
	implementation bool
	sessionID      string
	logUsage       *tokens.Usage
}

type planTokenUsageSnapshot struct {
	usage                *tokens.Usage
	sessionID            string
	providerSessionIDs   []string
	fetchProviderSession []bool
	expectedSessions     int
	wasResumed           bool
}

func buildPlanTokenUsageSnapshot(
	job *storage.ReviewJob,
	phases []planTokenPhase,
) planTokenUsageSnapshot {
	resolvedPhases := append([]planTokenPhase(nil), phases...)
	for i := range resolvedPhases {
		phase := &resolvedPhases[i]
		if !phase.invoked {
			continue
		}
		if phase.sessionID == "" && phase.logUsage != nil {
			phase.sessionID = phase.logUsage.ThreadID
		}
		if phase.implementation && phase.sessionID == "" {
			phase.sessionID = job.SessionID
		}
	}

	implementationSessionID := ""
	for _, phase := range resolvedPhases {
		if phase.implementation {
			implementationSessionID = phase.sessionID
			break
		}
	}
	if implementationSessionID == "" {
		implementationSessionID = job.SessionID
	}
	wasResumed := job.SessionID != "" && implementationSessionID == job.SessionID

	providerSessionIDs := make([]string, 0, len(resolvedPhases))
	fetchProviderSession := make([]bool, len(resolvedPhases))
	seenSessions := make(map[string]bool, len(resolvedPhases))
	expectedSessions := 0
	for i, phase := range resolvedPhases {
		if !phase.invoked {
			continue
		}
		expectedSessions++
		isFreshProviderSession := !phase.implementation || !wasResumed
		if phase.sessionID != "" && isFreshProviderSession && !seenSessions[phase.sessionID] {
			seenSessions[phase.sessionID] = true
			providerSessionIDs = append(providerSessionIDs, phase.sessionID)
			fetchProviderSession[i] = true
		}
	}

	phaseLogs := make([]*tokens.Usage, 0, len(resolvedPhases))
	for _, phase := range resolvedPhases {
		if phase.invoked {
			phaseLogs = append(phaseLogs, phase.logUsage)
		}
	}
	usage := aggregateTokenUsages(phaseLogs)
	if usage == nil {
		if expectedSessions == 0 {
			return planTokenUsageSnapshot{
				providerSessionIDs: providerSessionIDs, fetchProviderSession: fetchProviderSession,
				expectedSessions: expectedSessions, wasResumed: wasResumed,
			}
		}
		usage = &tokens.Usage{}
	}
	threadID := implementationSessionID
	if threadID == "" {
		for _, phase := range slices.Backward(resolvedPhases) {
			if phase.sessionID != "" {
				threadID = phase.sessionID
				break
			}
		}
	}
	usage.ThreadID = threadID
	usage.ProviderSessionIDs = providerSessionIDs
	usage.ExpectedProviderSessions = expectedSessions
	if expectedSessions > len(providerSessionIDs) {
		usage.HasCost = false
	}
	for _, phase := range slices.Backward(resolvedPhases) {
		if phase.logUsage != nil {
			usage.EventOffset = phase.logUsage.EventOffset
			break
		}
	}
	return planTokenUsageSnapshot{
		usage: usage, sessionID: threadID, providerSessionIDs: providerSessionIDs,
		fetchProviderSession: fetchProviderSession, expectedSessions: expectedSessions,
		wasResumed: wasResumed,
	}
}

func aggregateTokenUsages(usages []*tokens.Usage) *tokens.Usage {
	return backfill.AggregateTokenUsages(usages)
}
