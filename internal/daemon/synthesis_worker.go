package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

// errSynthesisCanceled signals that the synthesis agent run was canceled, so the
// caller must not store a review (the job is already terminal).
var errSynthesisCanceled = errors.New("synthesis canceled")

// processSynthesisJob executes a panel synthesis job against the run's member
// reviews. It picks one of three branches: all members failed -> durable fail
// review (no agent); exactly one member succeeded -> passthrough that member's
// output unless min-severity filtering requires a synthesis pass; two or more
// succeeded -> a single verify+dedupe agent call.
func (wp *WorkerPool) processSynthesisJob(
	ctx context.Context, workerID string, job *storage.ReviewJob,
) {
	rows, err := wp.db.GetPanelMemberReviews(job.PanelRunUUID)
	if err != nil {
		// A storage error must NOT masquerade as an all-failed synthesized
		// review. Use the non-agent retry/fail path: a DB read failure is not an
		// agent fault, so it must not trigger backup-agent failover (a different
		// agent cannot fix a storage error).
		wp.failOrRetry(workerID, job, job.Agent, fmt.Sprintf("load panel members: %v", err))
		return
	}
	results := toReviewResults(rows)
	succeeded := filterSucceeded(results)

	switch len(succeeded) {
	case 0:
		// Every member failed — emit a durable fail review with no agent call.
		// The comment renders the head SHA (FormatAllFailedComment short-SHAs its
		// arg), so pass the head side of the frozen mergeBase..headSHA range.
		wp.completeSynthesis(workerID, job, job.Agent, "",
			reviewpkg.FormatAllFailedComment(results, headOf(job.GitRef)))
	case 1:
		// Exactly one member produced output — pass it through verbatim and
		// label the review with that member's agent when no panel-level severity
		// filter needs to be applied, or when the member already passed and
		// there are no findings to filter.
		if config.IsMarkerOnlyOutput(succeeded[0].Output) {
			wp.completeSynthesis(workerID, job, succeeded[0].Agent, "", "No issues found.")
			return
		}
		if !singleSuccessCanPassthrough(job.MinSeverity) &&
			storage.ParseVerdict(succeeded[0].Output) != "P" {
			wp.synthesizeSucceededResults(ctx, workerID, job, succeeded)
			return
		}
		wp.completeSynthesis(workerID, job, succeeded[0].Agent, "", succeeded[0].Output)
	default:
		if allMembersPassed(results, succeeded) {
			wp.completeSynthesis(workerID, job, job.Agent, "", "No issues found.")
			return
		}
		// Two or more succeeded — combine and deduplicate via one agent call.
		wp.synthesizeSucceededResults(ctx, workerID, job, succeeded)
	}
}

func singleSuccessCanPassthrough(minSeverity string) bool {
	switch strings.ToLower(strings.TrimSpace(minSeverity)) {
	case "", "low":
		return true
	default:
		return false
	}
}

func (wp *WorkerPool) synthesizeSucceededResults(
	ctx context.Context,
	workerID string,
	job *storage.ReviewJob,
	succeeded []reviewpkg.ReviewResult,
) {
	// Mirror processJob's quota gate: an agent already in cooldown must fail
	// over instead of burning another quota-exhausted call. The no-agent
	// branches skip this check because they never invoke an agent.
	canonicalAgent := agent.CanonicalName(job.Agent)
	if wp.isAgentCoolingDown(canonicalAgent) {
		wp.failoverOrFail(workerID, job, canonicalAgent,
			fmt.Sprintf("agent %s quota cooldown active", canonicalAgent))
		return
	}
	prompt := reviewpkg.BuildSynthesisPrompt(succeeded, job.MinSeverity)
	out, resolvedAgent, runErr := wp.runSynthesisAgent(ctx, workerID, job, prompt)
	if runErr != nil {
		// runSynthesisAgent already handled the failure/cancel.
		return
	}
	wp.completeSynthesis(workerID, job, resolvedAgent, prompt, out)
}

// headOf returns the head side of a git ref range: the part after the last
// ".." when present, else the ref unchanged. A panel synthesis job's GitRef is
// the frozen mergeBase..headSHA range; formatters short-SHA their argument, so
// callers that render a single SHA must pass the head side, not the whole range.
func headOf(gitRef string) string {
	if i := strings.LastIndex(gitRef, ".."); i >= 0 {
		return gitRef[i+2:]
	}
	return gitRef
}

// filterSucceeded keeps member results that completed with non-empty output.
func filterSucceeded(results []reviewpkg.ReviewResult) []reviewpkg.ReviewResult {
	out := make([]reviewpkg.ReviewResult, 0, len(results))
	for _, r := range results {
		if r.Status == reviewpkg.ResultDone && strings.TrimSpace(r.Output) != "" {
			out = append(out, r)
		}
	}
	return out
}

// allMembersPassed reports whether every panel member completed successfully
// with a passing review. A clean panel does not need an extra agent synthesis
// pass: there are no findings to verify or deduplicate.
func allMembersPassed(
	results []reviewpkg.ReviewResult,
	succeeded []reviewpkg.ReviewResult,
) bool {
	if len(results) == 0 || len(results) != len(succeeded) {
		return false
	}
	for _, r := range succeeded {
		if storage.ParseVerdict(r.Output) != "P" {
			return false
		}
	}
	return true
}

// completeSynthesis stores the synthesis review, guards against the cancel race,
// and broadcasts review.completed. The done-path mirrors processJob's tail.
func (wp *WorkerPool) completeSynthesis(
	workerID string, job *storage.ReviewJob, agentName, prompt, output string,
) {
	if err := wp.db.CompleteJob(job.ID, agentName, prompt, output); err != nil {
		log.Printf("[%s] Error storing synthesis review for job %d: %v", workerID, job.ID, err)
		return
	}

	// CompleteJob no-ops when status != running (cancel race). Confirm the job
	// actually completed before broadcasting so downstream counters stay sane.
	j, err := wp.db.GetJobByID(job.ID)
	if err != nil {
		log.Printf("[%s] Synthesis job %d: failed to verify status: %v", workerID, job.ID, err)
		return
	}
	if j.Status != storage.JobStatusDone {
		log.Printf("[%s] Synthesis job %d not completed (status=%s), skipping broadcast",
			workerID, job.ID, j.Status)
		return
	}
	wp.autoClosePassingReview(workerID, job, output)

	log.Printf("[%s] Completed synthesis job %d %s panel=%s",
		workerID, job.ID, job.RepoName, job.PanelName)

	wp.broadcaster.Broadcast(Event{
		Type:     "review.completed",
		TS:       time.Now(),
		JobID:    job.ID,
		JobUUID:  job.UUID,
		Repo:     job.RepoPath,
		RepoName: job.RepoName,
		SHA:      job.GitRef,
		Branch:   job.HookBranch(),
		Agent:    agentName,
		Verdict:  storage.ParseVerdict(output),
		Findings: output,
	})
}

// runSynthesisAgent invokes the configured agent read-only (non-agentic) to
// combine and deduplicate member findings. It returns the agent output and the
// resolved agent name (which may differ from job.Agent after alias/fallback
// resolution, so the caller labels the stored review and completed broadcast
// consistently with the started broadcast). On failure it returns an error
// after routing through failOrRetryAgent; cancel returns errSynthesisCanceled
// so the caller stores nothing.
func (wp *WorkerPool) runSynthesisAgent(
	ctx context.Context, workerID string, job *storage.ReviewJob, prompt string,
) (string, string, error) {
	if err := wp.db.SaveJobPrompt(job.ID, prompt); err != nil {
		log.Printf("[%s] Error saving synthesis prompt for job %d: %v", workerID, job.ID, err)
	}

	a, agentName, err := wp.configureSynthesisAgent(workerID, job)
	if err != nil {
		return "", "", err
	}

	wp.broadcaster.Broadcast(Event{
		Type:     "review.started",
		TS:       time.Now(),
		JobID:    job.ID,
		Repo:     job.RepoPath,
		RepoName: job.RepoName,
		SHA:      job.GitRef,
		Branch:   job.HookBranch(),
		Agent:    agentName,
	})

	normalizer := GetNormalizer(agentName)
	outputWriter := wp.outputBuffers.Writer(job.ID, normalizer)
	defer func() {
		outputWriter.Flush()
		wp.outputBuffers.CloseJob(job.ID)
	}()
	jobLog := newJobLogWriter(job.ID)
	defer func() {
		if cErr := jobLog.Close(); cErr != nil {
			log.Printf("[%s] Warning: close job log for job %d: %v", workerID, job.ID, cErr)
		}
	}()
	agentOutput := io.MultiWriter(jobLog, outputWriter)
	sessionWriter := agent.NewSessionCaptureWriter(agentOutput, func(sessionID string) {
		if err := wp.db.SaveJobSessionID(job.ID, workerID, sessionID); err != nil {
			log.Printf("[%s] Error saving session ID for synthesis job %d: %v", workerID, job.ID, err)
		}
	})
	agentOutput = sessionWriter

	var output string
	if synthAgent, ok := a.(agent.SynthesisAgent); ok {
		// No pre-agent gate on this path; mark immediately before the agent runs
		// to keep the "set only when an agent actually runs" invariant adjacent
		// to the call.
		wp.markAgentInvoked(workerID, job, a)
		output, err = synthAgent.Synthesize(ctx, prompt, agentOutput)
	} else {
		// Verify findings against the reviewed checkout: a panel enqueued from a
		// linked worktree must synthesize against that worktree, and CI panels
		// get a detached checkout at the reviewed head instead of the stale
		// shared clone.
		checkout, checkoutErr := wp.prepareJobCheckout(ctx, workerID, job)
		if checkout.cleanup != nil {
			defer checkout.cleanup()
		}
		if checkoutErr != nil {
			wp.failOrRetry(workerID, job, agentName, fmt.Sprintf("prepare checkout: %v", checkoutErr))
			return "", agentName, checkoutErr
		}
		// Checkout succeeded and the agent is about to run; mark it invoked only
		// now so a checkout failure above is never miscounted as an agent run.
		wp.markAgentInvoked(workerID, job, a)
		output, err = a.Review(ctx, checkout.agentRepoPath, job.GitRef, prompt, agentOutput)
	}
	sessionWriter.Flush()
	if sessionID := sessionWriter.SessionID(); sessionID != "" {
		if saveErr := wp.db.SaveJobSessionID(job.ID, workerID, sessionID); saveErr != nil {
			log.Printf("[%s] Error persisting session ID for synthesis job %d: %v", workerID, job.ID, saveErr)
		}
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			// Job was canceled mid-run; it is already terminal. Don't fail it.
			log.Printf("[%s] Synthesis job %d canceled during agent run", workerID, job.ID)
			return "", agentName, errSynthesisCanceled
		}
		wp.failOrRetryAgent(workerID, job, agentName, fmt.Sprintf("agent: %v", err))
		return "", agentName, err
	}
	wp.captureTokenUsageForSession(context.Background(), workerID, job, sessionWriter.SessionID())
	return output, agentName, nil
}

// configureSynthesisAgent resolves and configures the read-only synthesis agent,
// returning the agent and its resolved name. Failures route through
// failOrRetryAgent before returning the error.
func (wp *WorkerPool) configureSynthesisAgent(
	workerID string, job *storage.ReviewJob,
) (agent.Agent, string, error) {
	cfg := wp.cfgGetter.Config()
	baseAgent, err := agent.GetAvailableWithConfig(job.RepoPath, job.Agent, cfg, job.BackupAgent)
	if err != nil {
		wp.failOrRetryAgent(workerID, job, job.Agent, fmt.Sprintf("get agent: %v", err))
		return nil, "", err
	}

	reasoning := strings.ToLower(strings.TrimSpace(job.Reasoning))
	if reasoning == "" {
		reasoning = "thorough"
	}
	reasoningLevel := agent.ParseReasoningLevel(reasoning)

	model := job.Model
	if synthesisSelectedBackupAgent(job, baseAgent.Name(), cfg) {
		model = job.BackupModel
	}

	// Synthesis reads the repo to verify findings but must never edit it.
	a := applyCodexReviewSettings(
		baseAgent.WithReasoning(reasoningLevel).WithAgentic(false).WithModel(model),
		job, cfg,
	)
	if job.Provider != "" {
		if pa, ok := a.(*agent.PiAgent); ok {
			a = pa.WithProvider(job.Provider)
		}
	}

	agentName := a.Name()
	return a, agentName, nil
}

func synthesisSelectedBackupAgent(job *storage.ReviewJob, selectedAgent string, cfg *config.Config) bool {
	if !synthesisAgentNameMatchesWithConfig(selectedAgent, job.BackupAgent, job, cfg) {
		return false
	}
	return !synthesisAgentNameMatchesWithConfig(selectedAgent, job.Agent, job, cfg)
}

func synthesisAgentNameMatches(selectedAgent, configuredAgent string) bool {
	selectedAgent = strings.TrimSpace(selectedAgent)
	configuredAgent = strings.TrimSpace(configuredAgent)
	if selectedAgent == "" || configuredAgent == "" {
		return false
	}
	if agent.CanonicalName(selectedAgent) == agent.CanonicalName(configuredAgent) {
		return true
	}
	resolvedConfigured, err := agent.Get(configuredAgent)
	if err != nil {
		return false
	}
	return agent.CanonicalName(selectedAgent) == agent.CanonicalName(resolvedConfigured.Name())
}

func synthesisAgentNameMatchesWithConfig(
	selectedAgent, configuredAgent string, job *storage.ReviewJob, cfg *config.Config,
) bool {
	if synthesisAgentNameMatches(selectedAgent, configuredAgent) {
		return true
	}
	if agent.CanonicalName(selectedAgent) != "acp" {
		return false
	}
	acpCfg := config.ResolveACPAgentConfig(job.RepoPath, cfg)
	if acpCfg == nil {
		return false
	}
	return strings.TrimSpace(configuredAgent) == strings.TrimSpace(acpCfg.Name)
}
