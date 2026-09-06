package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/agent"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

// errSynthesisCanceled signals that the synthesis agent run was canceled, so the
// caller must not store a review. A user-canceled job is terminal; an
// update-interrupted attempt has already returned to the queue.
var errSynthesisCanceled = errors.New("synthesis canceled")

// processSynthesisJob executes a panel synthesis job against the run's member
// reviews. It picks one of three branches: all members failed -> durable fail
// review (no agent); exactly one member succeeded -> passthrough that member's
// output unless min-severity filtering requires a synthesis pass; two or more
// succeeded -> a single verify+dedupe agent call.
func (wp *WorkerPool) processSynthesisJob(
	ctx context.Context, workerID string, job *storage.ReviewJob,
) {
	if job.PanelRunUUID == nil {
		wp.failOrRetryContext(ctx, workerID, job, job.Agent, "synthesis job has no panel run UUID")
		return
	}
	rows, err := wp.db.GetPanelMemberReviews(*job.PanelRunUUID)
	if err != nil {
		// A storage error must NOT masquerade as an all-failed synthesized
		// review. Use the non-agent retry/fail path: a DB read failure is not an
		// agent fault, so it must not trigger backup-agent failover (a different
		// agent cannot fix a storage error).
		wp.failOrRetryContext(ctx, workerID, job, job.Agent, fmt.Sprintf("load panel members: %v", err))
		return
	}
	results := toReviewResults(rows)
	for i := range results {
		results[i] = results[i].ApplyMinSeverity(job.MinSeverity)
	}
	succeeded := filterSucceeded(results)

	switch len(succeeded) {
	case 0:
		if errMsg, ok := allAvailabilitySkippedFailure(results); ok {
			wp.failSynthesisWithoutReviewContext(ctx, workerID, job, errMsg)
			return
		}
		// Every member failed — emit a durable fail review with no agent call.
		// The comment renders the head SHA (FormatAllFailedComment short-SHAs its
		// arg), so pass the head side of the frozen mergeBase..headSHA range.
		wp.completeSynthesisContext(workerID, job, synthesisResult{
			agentName: job.Agent,
			output:    reviewpkg.FormatAllFailedComment(results, headOf(job.GitRef)),
			verdict:   storage.VerdictFail,
		})
	case 1:
		// Exactly one member produced output — pass it through verbatim and
		// label the review with that member's agent. Its verdict already
		// honors the panel threshold, so no synthesis agent is needed.
		wp.completeSynthesisContext(workerID, job, synthesisResult{
			agentName:        succeeded[0].Agent,
			output:           succeeded[0].Output,
			verdict:          succeeded[0].Verdict,
			structuredOutput: succeeded[0].StructuredOutput,
		})
	default:
		if allMembersPassed(results, succeeded) && !anyRetainedFindings(succeeded) {
			wp.completeSynthesisContext(workerID, job, synthesisResult{
				agentName: job.Agent,
				output:    "No issues found.",
				verdict:   storage.VerdictPass,
			})
			return
		}
		// Two or more succeeded — combine and deduplicate via one agent call.
		wp.synthesizeSucceededResults(ctx, workerID, job, succeeded)
	}
}

func allAvailabilitySkippedFailure(results []reviewpkg.ReviewResult) (string, bool) {
	if len(results) == 0 {
		return "", false
	}
	first := ""
	for _, r := range results {
		if reviewpkg.IsQuotaFailure(r) || reviewpkg.IsTransientFailure(r) {
			if first == "" {
				first = r.Error
			}
			continue
		}
		return "", false
	}
	if first == "" {
		first = reviewpkg.OutageError("all review agents unavailable")
	}
	return first, true
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
		wp.failCooldownOrFailoverContext(ctx, workerID, job, canonicalAgent,
			fmt.Sprintf("agent %s quota cooldown active", canonicalAgent))
		return
	}
	prompt := reviewpkg.BuildSynthesisPrompt(succeeded, job.MinSeverity)
	doc, resolvedAgent, capturedSession, runErr := wp.runSynthesisAgent(
		ctx, workerID, job, succeeded, prompt,
	)
	if runErr != nil {
		// runSynthesisAgent already handled the failure/cancel.
		return
	}
	structured, err := json.Marshal(doc)
	if err != nil {
		log.Printf("[%s] Error encoding synthesis document for job %d: %v", workerID, job.ID, err)
	}
	wp.completeSynthesisContext(workerID, job, synthesisResult{
		agentName:        resolvedAgent,
		prompt:           prompt,
		output:           doc.Markdown(job.MinSeverity),
		verdict:          reviewpkg.SynthesisVerdict(doc, job.MinSeverity),
		structuredOutput: structured,
		capturedSession:  capturedSession,
		captureUsage:     true,
	})
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

// filterSucceeded keeps member results that completed with substantive output.
func filterSucceeded(results []reviewpkg.ReviewResult) []reviewpkg.ReviewResult {
	out := make([]reviewpkg.ReviewResult, 0, len(results))
	for _, r := range results {
		if reviewpkg.IsSubstantiveOutput(r) {
			out = append(out, r)
		}
	}
	return out
}

// anyRetainedFindings reports whether a passing member still carries findings
// below the panel threshold. Those must reach the synthesized output, so the
// clean-panel shortcut is not allowed to replace them with "No issues found."
func anyRetainedFindings(succeeded []reviewpkg.ReviewResult) bool {
	for _, r := range succeeded {
		if r.Structured != nil {
			if len(r.Structured.Findings) > 0 {
				return true
			}
			continue
		}
		if storage.HighestSeverityLabel(r.Output) != "" {
			return true
		}
	}
	return false
}

// allMembersPassed reports whether every panel member completed successfully
// with a passing review. A clean panel does not need an extra agent synthesis
// pass: there are no findings to verify or deduplicate.
func allMembersPassed(
	results []reviewpkg.ReviewResult,
	succeeded []reviewpkg.ReviewResult,
) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range succeeded {
		if !r.Passed() {
			return false
		}
	}
	ignored := 0
	for _, r := range results {
		if r.AllowFailure && (r.Status == reviewpkg.ResultFailed || r.Status == "canceled") {
			ignored++
		}
	}
	return len(results)-ignored == len(succeeded)
}

func (wp *WorkerPool) failSynthesisWithoutReviewContext(
	_ context.Context, workerID string, job *storage.ReviewJob, errorMsg string,
) {
	wp.runAttemptTransition(workerID, job, func() {
		wp.failSynthesisWithoutReviewLocked(workerID, job, errorMsg)
	})
}

func (wp *WorkerPool) failSynthesisWithoutReviewLocked(
	workerID string, job *storage.ReviewJob, errorMsg string,
) {
	if updated, err := wp.db.FailJob(job.ID, workerID, errorMsg); err != nil {
		log.Printf("[%s] Error failing skipped synthesis job %d: %v", workerID, job.ID, err)
	} else if updated {
		log.Printf("[%s] Synthesis job %d skipped because all panel members were unavailable",
			workerID, job.ID)
		wp.broadcastFailed(job, job.Agent, errorMsg)
		if wp.errorLog != nil {
			wp.errorLog.LogError("worker",
				fmt.Sprintf("synthesis job %d skipped: %s", job.ID, errorMsg),
				job.ID)
		}
		wp.logJobFailed(job.ID, workerID, job.Agent, errorMsg)
	}
}

// synthesisResult carries what a synthesis attempt produced. capturedSession
// and captureUsage are set only when a synthesis agent actually ran: usage
// capture must happen after the terminal write but before the completion
// broadcast so a CI cost footer never renders an unpriced synthesis row.
type synthesisResult struct {
	agentName string
	prompt    string
	output    string
	verdict   storage.Verdict
	// structuredOutput carries a passed-through member's findings document
	// so the synthesis review is as machine-readable as the member review.
	structuredOutput json.RawMessage
	capturedSession  string
	captureUsage     bool
}

// completeSynthesisContext stores the synthesis review, guards against the
// cancel race, and broadcasts review.completed. The done-path mirrors
// processJob's tail.
func (wp *WorkerPool) completeSynthesisContext(
	workerID string, job *storage.ReviewJob, res synthesisResult,
) {
	wp.runAttemptTransition(workerID, job, func() {
		wp.completeSynthesisLocked(workerID, job, res)
	})
}

func (wp *WorkerPool) completeSynthesisLocked(
	workerID string, job *storage.ReviewJob, res synthesisResult,
) {
	agentName, prompt, output := res.agentName, res.prompt, res.output
	var completeErr error
	if res.verdict != storage.VerdictUnknown {
		completeErr = wp.db.CompleteJobResult(
			job.ID, agentName, prompt, storage.ReviewCompletion{
				Output:           output,
				Verdict:          res.verdict,
				StructuredOutput: res.structuredOutput,
				MinSeverity:      job.MinSeverity,
			},
		)
	} else {
		completeErr = wp.db.CompleteJob(job.ID, agentName, prompt, output)
	}
	if completeErr != nil {
		// Leaving the job running would strand the panel with no comment.
		// Route the storage failure through the ordinary retry/fail path.
		log.Printf("[%s] Error storing synthesis review for job %d: %v", workerID, job.ID, completeErr)
		wp.failOrRetryInnerLocked(
			workerID, job, agentName,
			fmt.Sprintf("store synthesis review: %v", completeErr), false, nil,
		)
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
	if res.captureUsage {
		wp.captureTokenUsageForSession(
			context.Background(), workerID, job, res.capturedSession,
		)
	}
	verdict := res.verdict
	if verdict == storage.VerdictUnknown {
		verdict = storage.ParseVerdict(output)
	}
	wp.autoClosePassingReview(workerID, job, verdict)

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
		Verdict:  string(verdict),
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
	ctx context.Context, workerID string, job *storage.ReviewJob,
	reviews []reviewpkg.ReviewResult, prompt string,
) (reviewpkg.SynthesisDocument, string, string, error) {
	if err := wp.db.SaveJobPrompt(job.ID, prompt); err != nil {
		log.Printf("[%s] Error saving synthesis prompt for job %d: %v", workerID, job.ID, err)
	}

	a, agentName, err := wp.configureSynthesisAgentContext(ctx, workerID, job)
	if err != nil {
		return reviewpkg.SynthesisDocument{}, "", "", err
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
	jobLog := newAgentJobLogWriter(job.ID, agentName)
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

	doc, err := reviewpkg.RunSynthesisAgent(ctx, a, reviews, prompt, job.MinSeverity, agentOutput, reviewpkg.SynthesisHooks{
		// Mark the agent invoked only once it is about to run, so a checkout
		// failure below is never miscounted as an agent run.
		BeforeInvoke: func() { wp.markAgentInvoked(workerID, job, a) },
		// Verify findings against the reviewed checkout: a panel enqueued from a
		// linked worktree must synthesize against that worktree, and CI panels
		// get a detached checkout at the reviewed head instead of the stale
		// shared clone.
		Checkout: func() (reviewpkg.SynthesisCheckout, error) {
			checkout, err := wp.prepareJobCheckout(ctx, workerID, job)
			return reviewpkg.SynthesisCheckout{
				RepoPath: checkout.agentRepoPath,
				GitRef:   job.GitRef,
				Cleanup:  checkout.cleanup,
			}, err
		},
	})
	sessionWriter.Flush()
	if sessionID := sessionWriter.SessionID(); sessionID != "" {
		if saveErr := wp.db.SaveJobSessionID(job.ID, workerID, sessionID); saveErr != nil {
			log.Printf("[%s] Error persisting session ID for synthesis job %d: %v", workerID, job.ID, saveErr)
		}
	}
	if err != nil {
		if checkoutErr, ok := errors.AsType[*reviewpkg.SynthesisCheckoutError](err); ok {
			wp.failOrRetryContext(ctx, workerID, job, agentName, checkoutErr.Error())
			return reviewpkg.SynthesisDocument{}, agentName, "", checkoutErr.Err
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			if wp.handleUpdateInterruption(ctx, workerID, job) {
				return reviewpkg.SynthesisDocument{}, agentName, "", errSynthesisCanceled
			}
			// A user cancellation is already terminal. Don't fail it.
			log.Printf("[%s] Synthesis job %d canceled during agent run", workerID, job.ID)
			return reviewpkg.SynthesisDocument{}, agentName, "", errSynthesisCanceled
		}
		if noVerdict, ok := errors.AsType[*reviewpkg.NoVerdictError](err); ok {
			// Same code and same failover as a review that produced no
			// verdict: retrying the same agent cannot change the outcome.
			wp.failoverOrFailNonRetryableAgentContext(
				ctx, workerID, job, agentName, reviewpkg.NoVerdictMessage(noVerdict),
			)
			return reviewpkg.SynthesisDocument{}, agentName, sessionWriter.SessionID(), err
		}
		wp.failOrRetryAgentContext(ctx, workerID, job, agentName, fmt.Sprintf("agent: %v", err))
		return reviewpkg.SynthesisDocument{}, agentName, sessionWriter.SessionID(), err
	}
	if wp.handleUpdateInterruption(ctx, workerID, job) {
		return reviewpkg.SynthesisDocument{}, agentName, "", errSynthesisCanceled
	}
	return doc, agentName, sessionWriter.SessionID(), nil
}

// configureSynthesisAgent resolves and configures the read-only synthesis agent,
// returning the agent and its resolved name. Failures route through
// failOrRetryAgent before returning the error.
func (wp *WorkerPool) configureSynthesisAgent(
	workerID string, job *storage.ReviewJob,
) (agent.Agent, string, error) {
	return wp.configureSynthesisAgentContext(context.Background(), workerID, job)
}

func (wp *WorkerPool) configureSynthesisAgentContext(
	ctx context.Context, workerID string, job *storage.ReviewJob,
) (agent.Agent, string, error) {
	cfg := wp.cfgGetter.Config()
	baseAgent, err := resolveConfiguredJobAgent(job, cfg, job.BackupAgent)
	if err != nil {
		wp.failOrRetryAgentContext(ctx, workerID, job, job.Agent, fmt.Sprintf("get agent: %v", err))
		return nil, "", err
	}

	reasoning := strings.ToLower(strings.TrimSpace(job.Reasoning))
	if reasoning == "" {
		reasoning = "thorough"
	}
	reasoningLevel := agent.ParseReasoningLevel(reasoning)

	model := job.Model
	if synthesisSelectedBackupAgent(job, baseAgent.Name()) {
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

func synthesisSelectedBackupAgent(job *storage.ReviewJob, selectedAgent string) bool {
	if !synthesisAgentNameMatches(selectedAgent, job.BackupAgent) {
		return false
	}
	return !synthesisAgentNameMatches(selectedAgent, job.Agent)
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
