package daemon

import (
	"context"
	"encoding/json/v2"
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
	"go.kenn.io/roborev/internal/tokens"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// errSynthesisCanceled signals that the synthesis agent run was canceled, so the
// caller must not store a review. A user-canceled job is terminal; an
// update-interrupted attempt has already returned to the queue.
var errSynthesisCanceled = errors.New("synthesis canceled")

// processSynthesisJob executes a panel synthesis job against the run's member
// reviews. It picks one of three branches: all members failed -> failed job
// without a review (no agent); exactly one member succeeded and the panel has
// no synthesis instructions -> passthrough that member's output and effective
// threshold; otherwise -> a single verify+dedupe agent call.
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
	// Keep the configured job immutable while carrying the effective policy
	// through every synthesis outcome and its persisted completion.
	resolvedJob := *job
	resolvedJob.MinSeverity = reviewpkg.ResolveSynthesisMinSeverity(results, job.MinSeverity)
	job = &resolvedJob
	// Decide framing from every voting member before failures are dropped.
	neutral := reviewpkg.NeedsNeutralSynthesis(results)
	succeeded := filterSucceeded(results)
	cfg, err := synthesisConfig(job)
	if err != nil {
		wp.failOrRetryContext(ctx, workerID, job, job.Agent, err.Error())
		return
	}

	switch {
	case len(succeeded) == 0:
		errMsg, ok := allAvailabilitySkippedFailure(results)
		if !ok {
			errMsg = "all review agents failed or produced no usable output"
		}
		wp.failSynthesisWithoutReviewContext(ctx, workerID, job, errMsg)
	case len(succeeded) == 1 && cfg.Instructions == "":
		// Exactly one member produced output — pass it through verbatim and
		// label the review with that member's agent. Its verdict already
		// honors the panel threshold, so no synthesis agent is needed.
		// With synthesis instructions, the default branch runs the agent so it
		// can act on the findings.
		single := succeeded[0]
		doc, err := reviewpkg.DecodeStructuredReview(single.StructuredOutput)
		if err != nil {
			wp.failSynthesisWithoutReviewContext(ctx, workerID, job, err.Error())
			return
		}
		doc.SourceLabels = reviewpkg.SynthesisSourceLabels(succeeded)
		for i := range doc.Findings {
			doc.Findings[i].Sources = []int{1}
		}
		single.StructuredOutput, err = json.Marshal(doc)
		if err != nil {
			wp.failSynthesisWithoutReviewContext(ctx, workerID, job, err.Error())
			return
		}
		single.Output = doc.Markdown(job.MinSeverity)
		wp.checkAndCompleteSynthesis(ctx, workerID, job, synthesisResult{review: single}, doc, cfg.Check)
	default:
		if allMembersPassed(results, succeeded) && !anyRetainedFindings(succeeded) {
			wp.completeSynthesisDocument(workerID, job, structuredreview.Document{
				SchemaVersion: structuredreview.SchemaVersion, Summary: "No issues found.",
				Verdict: structuredreview.VerdictPass, Findings: []structuredreview.Finding{},
			})
			return
		}
		// Combine and deduplicate via one agent call.
		wp.synthesizeSucceededResults(ctx, workerID, job, succeeded, neutral, cfg.Instructions, cfg.Check)
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
	neutral bool,
	instructions string,
	check *ciCheckConfig,
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
	prompt := reviewpkg.BuildSynthesisPrompt(succeeded, neutral, instructions)
	doc, resolvedAgent, capturedSession, runErr := wp.runSynthesisAgent(
		ctx, workerID, job, succeeded, prompt,
	)
	if runErr != nil {
		// runSynthesisAgent already handled the failure/cancel.
		return
	}
	result, err := synthesisDocumentResult(job, doc)
	if err != nil {
		log.Printf("[%s] Error encoding synthesis document for job %d: %v", workerID, job.ID, err)
	}
	result.Agent = resolvedAgent
	wp.checkAndCompleteSynthesis(ctx, workerID, job, synthesisResult{
		review:          result,
		capturedSession: capturedSession,
		captureUsage:    true,
	}, doc, check)
}

// synthesisJobConfig is the snapshot a panel synthesis job stores in its
// panel_member_config_json column. CI synthesis jobs also freeze their ACP
// execution config there, which ciACPRepoConfig reads.
type synthesisJobConfig struct {
	ciACPExecutionConfig
	Instructions string         `json:"instructions,omitempty"`
	Check        *ciCheckConfig `json:"check,omitzero"`
}

func synthesisExperimentConfig(snapshot string) (string, error) {
	c, err := synthesisConfig(&storage.ReviewJob{JobType: storage.JobTypeSynthesis, PanelMemberConfigJSON: snapshot})
	if err != nil {
		return "", err
	}
	if c.Check != nil {
		c.Check.PullRequest = ""
	}
	return c.encode()
}

func restoreSynthesisExperimentConfig(frozen, snapshot string) (string, error) {
	c, err := synthesisConfig(&storage.ReviewJob{JobType: storage.JobTypeSynthesis, PanelMemberConfigJSON: frozen})
	if err != nil {
		return "", err
	}
	job, err := synthesisConfig(&storage.ReviewJob{JobType: storage.JobTypeSynthesis, PanelMemberConfigJSON: snapshot})
	if err != nil {
		return "", err
	}
	if c.Check != nil {
		c.Check.PullRequest = ""
		if job.Check != nil {
			c.Check.PullRequest = job.Check.PullRequest
		}
	}
	return c.encode()
}

// encode returns "" when nothing is set, so a synthesis job without
// instructions, a checker, or an ACP snapshot stores no config.
func (c synthesisJobConfig) encode() (string, error) {
	if c.Instructions == "" && len(c.ACP) == 0 && c.Check == nil {
		return "", nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// synthesisInstructions returns the panel synthesis instructions frozen with
// the job, or "" when it has none. Only a synthesis job's config holds them:
// a member's config uses the same key for its own reviewer instructions.
func synthesisInstructions(job *storage.ReviewJob) (string, error) {
	cfg, err := synthesisConfig(job)
	return cfg.Instructions, err
}

func synthesisConfig(job *storage.ReviewJob) (synthesisJobConfig, error) {
	if !job.IsSynthesisJob() || job.PanelMemberConfigJSON == "" {
		return synthesisJobConfig{}, nil
	}
	var c synthesisJobConfig
	if err := json.Unmarshal([]byte(job.PanelMemberConfigJSON), &c); err != nil {
		return synthesisJobConfig{}, fmt.Errorf("decode synthesis config: %w", err)
	}
	return c, nil
}

// headOf returns the head side of a git ref range: the part after the last
// ".." when present, else the ref unchanged. A panel synthesis job's GitRef is
// the frozen mergeBase..headSHA range; formatters short-SHA their argument, so
// callers that render a single SHA must pass the head side, not the whole range.
func headOf(gitRef string) string {
	if _, after, ok := strings.CutLast(gitRef, ".."); ok {
		return after
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
	if updated, err := wp.failJobAndInvalidateBudget(job.ID, workerID, errorMsg); err != nil {
		log.Printf("[%s] Error failing synthesis job %d: %v", workerID, job.ID, err)
	} else if updated {
		log.Printf("[%s] Synthesis job %d failed: %s",
			workerID, job.ID, errorMsg)
		// No synthesis agent ran. Keep CI and streaming subscribers informed
		// without attributing another agent failure or repeating member alerts.
		event := eventForJob("review.failed", job, job.ID)
		event.Agent = ""
		event.Error = errorMsg
		event.SuppressHooks = true
		wp.broadcaster.Broadcast(event)
		if wp.errorLog != nil {
			wp.errorLog.LogError("worker",
				fmt.Sprintf("synthesis job %d failed: %s", job.ID, errorMsg),
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
	// Keep the review and its effective policy together across completion.
	review          reviewpkg.ReviewResult
	capturedSession string
	captureUsage    bool
	checkerUsage    *tokens.Usage
}

// completeSynthesisContext stores the synthesis review, guards against the
// cancel race, and broadcasts review.completed. The done-path mirrors
// processJob's tail.
func (wp *WorkerPool) completeSynthesisContext(
	workerID string, job *storage.ReviewJob, res synthesisResult,
) {
	var tokenUsage *sessionTokenUsage
	if res.captureUsage || res.checkerUsage != nil {
		collected := wp.collectTokenUsageForSession(
			context.Background(), workerID, job, res.capturedSession,
		)
		collected.withUnpricedPhase(res.captureUsage, res.checkerUsage)
		tokenUsage = &collected
	}
	wp.runAttemptTransition(workerID, job, func() {
		wp.completeSynthesisLocked(workerID, job, res, tokenUsage)
	})
}

func (wp *WorkerPool) completeSynthesisLocked(
	workerID string, job *storage.ReviewJob, res synthesisResult, tokenUsage *sessionTokenUsage,
) {
	agentName, output := res.review.Agent, res.review.Output
	completeErr := wp.db.CompleteJobResult(job.ID, agentName, storage.ReviewCompletion{
		Output: output, Verdict: res.review.Verdict, StructuredOutput: res.review.StructuredOutput, MinSeverity: res.review.MinSeverity,
	})
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
	wp.invalidateBudgetSpend()
	if tokenUsage != nil {
		wp.storeTokenUsageForSession(workerID, job, *tokenUsage)
	}
	verdict := res.review.Verdict
	if verdict == storage.VerdictUnknown && len(res.review.StructuredOutput) == 0 {
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
	if saved, err := wp.db.SaveJobPromptForAttempt(job, workerID, prompt); err != nil {
		log.Printf("[%s] Error saving synthesis prompt for job %d: %v", workerID, job.ID, err)
		wp.failOrRetryContext(ctx, workerID, job, job.Agent, fmt.Sprintf("save synthesis prompt: %v", err))
		return reviewpkg.SynthesisDocument{}, "", "", err
	} else if !saved {
		return reviewpkg.SynthesisDocument{}, "", "", errSynthesisCanceled
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
	defer outputWriter.Flush()
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

	cfg := wp.cfgGetter.Config()
	doc, err := reviewpkg.RunSynthesisAgent(ctx, a, reviews, prompt, job.MinSeverity, agentOutput, reviewpkg.SynthesisHooks{
		ConfigRepoPath: resolveEffectiveRepoPath(workerID, job),
		GlobalConfig:   cfg,
		// Mark the agent invoked only once it is about to run, so a checkout
		// failure below is never miscounted as an agent run.
		BeforeInvoke: func() { wp.markAgentInvoked(workerID, job, a) },
		// Synthesis follows the same isolation policy as its member reviews.
		Checkout: func() (reviewpkg.SynthesisCheckout, error) {
			checkout, err := wp.prepareJobCheckout(ctx, workerID, job, cfg)
			if err != nil {
				return reviewpkg.SynthesisCheckout{}, err
			}
			configRepoPath := ""
			if checkout.isolatedLocalReview {
				if err := wp.db.IsolateJobSession(job.ID, workerID); err != nil {
					checkout.cleanup()
					return reviewpkg.SynthesisCheckout{}, fmt.Errorf("clear synthesis session: %w", err)
				}
				job.SessionID = ""
				job.ResumeSourceJobUUID = nil
				checkout = wp.refreshIsolatedReviewConfigPath(workerID, checkout, job)
				configRepoPath = checkout.resolvedConfigRepoPath()
			}
			return reviewpkg.SynthesisCheckout{
				RepoPath:       checkout.agentRepoPath,
				ConfigRepoPath: configRepoPath,
				GitRef:         job.GitRef,
				Cleanup:        checkout.cleanup,
			}, nil
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

func (wp *WorkerPool) completeSynthesisDocument(workerID string, job *storage.ReviewJob, doc structuredreview.Document) {
	result, err := synthesisDocumentResult(job, doc)
	if err != nil {
		wp.failSynthesisWithoutReviewContext(context.Background(), workerID, job, err.Error())
		return
	}
	wp.completeSynthesisContext(workerID, job, synthesisResult{review: result})
}

func synthesisDocumentResult(job *storage.ReviewJob, doc structuredreview.Document) (reviewpkg.ReviewResult, error) {
	raw, err := json.Marshal(doc)
	verdict := storage.VerdictUnknown
	if !doc.UnableToReview() {
		verdict = reviewpkg.SynthesisVerdict(doc, job.MinSeverity)
	}
	return reviewpkg.ReviewResult{
		Agent: job.Agent, Output: doc.Markdown(job.MinSeverity), Verdict: verdict,
		StructuredOutput: raw, MinSeverity: job.MinSeverity,
	}, err
}

// needsExactCheckout reports whether a local committed review must run in a
// temporary checkout of the reviewed commit instead of the working tree:
// isolate_reviews asks for it, and synthesis instructions may tell the agent
// to read the code, which must be the code that was reviewed.
func needsExactCheckout(job *storage.ReviewJob, repoPath string, cfg *config.Config) (bool, error) {
	if config.ResolveIsolateReviews(repoPath, cfg) {
		return true, nil
	}
	instructions, err := synthesisInstructions(job)
	return instructions != "", err
}
