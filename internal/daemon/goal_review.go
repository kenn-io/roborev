package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

// goalReviewRunner is set only by package tests. Production leaves it nil and
// uses goalreview.RunPrepared's concrete safe-agent allowlist.
type goalReviewRunner func(context.Context, agent.Agent, string, goalreview.Snapshot, prompt.SnapshotResult, io.Writer) ([]goalreview.Finding, error)

func (s *Server) enqueueGoalReview(ctx context.Context, req EnqueueRequest) (*RawJSONOutput, error) {
	return s.enqueueGoalReviewWithConfig(ctx, req, "", nil)
}

func (s *Server) enqueueGoalReviewWithConfig(ctx context.Context, req EnqueueRequest, configRoot string, captured *goalreview.Snapshot) (*RawJSONOutput, error) {
	bad := func(err error) (*RawJSONOutput, error) {
		return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}
	if req.RepoPath == "" || req.CustomPrompt != "" || req.Agentic || req.GitRef != "" || req.CommitSHA != "" || req.Branch != "" || req.Since != "" || req.Panel != "" || req.DiffContent != "" || len(req.DirtyFiles) > 0 || req.MinSeverity != "" || req.OutputPrefix != "" || (req.JobType != "" && req.JobType != storage.JobTypeGoalReview) {
		return bad(fmt.Errorf("goal review requires repo_path and rejects commit selectors, panels, custom prompts and agentic inputs"))
	}
	root, err := gitrepo.Root(ctx, req.RepoPath)
	if err != nil {
		return bad(err)
	}
	if configRoot == "" {
		configRoot = root
	}
	mainRoot, err := gitrepo.MainRoot(ctx, root)
	if err != nil {
		return bad(err)
	}
	var snapshot goalreview.Snapshot
	if captured != nil {
		snapshot = *captured
	} else {
		repoConfig, err := config.LoadRepoConfig(configRoot)
		if err != nil {
			return bad(err)
		}
		selection, err := goalreview.Select(repoConfig, req.SpecFile, req.PlanFile)
		if err != nil {
			return bad(err)
		}
		snapshot, err = goalreview.Capture(ctx, root, selection, kata.NewCLIClient(configRoot))
		if err != nil {
			return bad(err)
		}
	}
	cfg := s.configWatcher.Config()
	a, reasoning, err := goalreview.ResolveAgent(configRoot, cfg, goalreview.AgentOptions{Agent: req.Agent, Model: req.Model, Provider: req.Provider, Reasoning: req.Reasoning})
	if err != nil {
		return bad(err)
	}
	resolution, err := agent.ResolveWorkflowConfig(req.Agent, configRoot, cfg, "review", reasoning)
	if err != nil {
		return bad(err)
	}
	repo, err := s.db.GetOrCreateRepo(mainRoot, config.ResolveRepoIdentity(mainRoot, nil))
	if err != nil {
		return rawJSONOutput(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	worktree := ""
	if filepath.Clean(root) != filepath.Clean(mainRoot) {
		worktree = root
	}
	job, err := s.db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: snapshot.ID(), Branch: gitrepo.CurrentBranch(ctx, root), Agent: a.Name(), Model: resolution.ModelForSelectedAgent(a.Name(), req.Model), Provider: req.Provider, RequestedModel: req.Model, RequestedProvider: req.Provider, Reasoning: reasoning, ReviewType: config.ReviewTypeGoal, JobType: storage.JobTypeGoalReview, Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true, Label: "Superpowers " + snapshot.Stage + " review", WorktreePath: worktree, Source: req.Source})
	if err != nil {
		return rawJSONOutput(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	job.RepoPath = repo.RootPath
	job.RepoName = repo.Name
	s.broadcaster.Broadcast(Event{Type: "goal_review.enqueued", TS: time.Now(), JobID: job.ID, JobUUID: job.UUID, Repo: repo.RootPath, RepoName: repo.Name, SHA: job.GitRef, Branch: job.Branch, Agent: job.Agent, WorktreePath: worktree})
	return rawJSONOutput(http.StatusCreated, EnqueueCreatedResponse{ReviewJob: job, UUID: *job.UUID})
}

func jobEventType(job *storage.ReviewJob, event string) string {
	if job != nil && job.IsGoalReviewJob() {
		return "goal_review." + event
	}
	return "review." + event
}

// GoalReviewConfigRepoPath selects the policy for a captured goal review.
// Watcher jobs use the registered main checkout; explicit requests use their
// checkout while it still exists.
func GoalReviewConfigRepoPath(job *storage.ReviewJob) string {
	if job.Source != "goal_watch" {
		if root := validatedWorktreePath(job.WorktreePath, job.RepoPath); root != "" {
			return root
		}
	}
	return job.RepoPath
}

func (wp *WorkerPool) processGoalReview(ctx context.Context, workerID string, job *storage.ReviewJob, cfg *config.Config) {
	root := job.RepoPath
	if worktreePath := validatedWorktreePath(job.WorktreePath, job.RepoPath); worktreePath != "" {
		root = worktreePath
	}
	fail := func(err error) { wp.failOrRetryContext(ctx, workerID, job, job.Agent, err.Error()) }
	snapshot, err := goalreview.ParseSnapshot(job.Prompt)
	if err != nil {
		fail(err)
		return
	}
	if snapshot.ID() != job.GitRef {
		fail(fmt.Errorf("frozen Superpowers snapshot digest mismatch"))
		return
	}
	prepared, err := prompt.NewBuilderWithConfig(wp.db, cfg).ForRepo(root, job.RepoID).Prepare(
		goalreview.BuildPrompt(snapshot),
		prompt.SnapshotTarget{RepoPath: root, ConfigRepoPath: GoalReviewConfigRepoPath(job)},
	)
	if err != nil {
		fail(fmt.Errorf("prepare goal review prompt: %w", err))
		return
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}
	a, err := resolveReviewJobAgent(job, cfg)
	if err != nil {
		fail(err)
		return
	}
	a = a.WithReasoning(agent.ParseReasoningLevel(job.Reasoning)).WithModel(job.Model)
	if job.Provider != "" {
		if pi, ok := a.(*agent.PiAgent); ok {
			a = pi.WithProvider(job.Provider)
		}
	}
	if err := goalreview.ValidateAgent(a); err != nil {
		fail(err)
		return
	}
	event := Event{Type: "goal_review.started", TS: time.Now(), JobID: job.ID, JobUUID: job.UUID, Repo: job.RepoPath, RepoName: job.RepoName, SHA: job.GitRef, Branch: job.Branch, Agent: a.Name(), WorktreePath: job.WorktreePath}
	wp.broadcaster.Broadcast(event)
	wp.markAgentInvoked(workerID, job, a)
	runner := wp.goalReviewRunner
	if runner == nil {
		runner = goalreview.RunPrepared
	}
	outputWriter := wp.outputBuffers.Writer(job.ID, GetNormalizer(a.Name()))
	defer outputWriter.Flush()
	jobLog := newAgentJobLogWriter(job.ID, a.Name())
	defer func() {
		if err := jobLog.Close(); err != nil {
			log.Printf("[%s] close goal review log: %v", workerID, err)
		}
		wp.captureGoalUsage(workerID, job, a.Name())
	}()
	findings, err := runner(ctx, a, root, snapshot, prepared, io.MultiWriter(jobLog, outputWriter))
	if ctx.Err() != nil {
		if current, getErr := wp.db.GetJobByID(job.ID); getErr == nil && current.Status == storage.JobStatusCanceled {
			event.Type = "goal_review.canceled"
			event.TS = time.Now()
			if !wp.cancellationEventOwnedByCaller(job.ID) {
				wp.broadcaster.Broadcast(event)
			}
			return
		}
	}
	if err != nil {
		if execution, ok := errors.AsType[*goalreview.ExecutionError](err); ok {
			wp.failOrRetryAgentExecutionContext(ctx, workerID, job, a.Name(), execution.Err)
		} else {
			fail(err)
		}
		return
	}
	output := goalreview.Render(findings)
	document := goalreview.Document(findings)
	raw, err := json.Marshal(document)
	if err != nil {
		fail(err)
		return
	}
	var completeErr error
	if wp.runAttemptTransition(workerID, job, func() {
		completeErr = wp.db.CompleteJobResult(job.ID, a.Name(), storage.ReviewCompletion{Output: output, Verdict: storage.VerdictFromPassed(len(findings) == 0), StructuredOutput: jsontext.Value(raw)})
	}) {
		return
	}
	if completeErr != nil {
		log.Printf("[%s] store goal review: %v", workerID, completeErr)
		return
	}
	current, err := wp.db.GetJobByID(job.ID)
	if err != nil || current.Status != storage.JobStatusDone {
		return
	}
	verdict := storage.ParseVerdict(output)
	wp.autoClosePassingReview(workerID, job, verdict)
	event.Type = "goal_review.completed"
	event.TS = time.Now()
	event.Verdict = string(verdict)
	event.Findings = output
	wp.broadcaster.Broadcast(event)
}

// Goal reviewers disable session persistence, so their own output is the
// source of usage. Store only the terminal attempt that produced this log.
func (wp *WorkerPool) captureGoalUsage(workerID string, job *storage.ReviewJob, agentName string) {
	file, err := os.Open(JobLogPath(job.ID))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("[%s] open goal review usage log: %v", workerID, err)
		return
	}
	defer file.Close()
	usage, err := tokens.ParseSchemaUsage(agentName, file)
	if err != nil {
		log.Printf("[%s] read goal review usage: %v", workerID, err)
		return
	}
	if usage == nil {
		return
	}
	updated, err := wp.db.BackfillJobTokenUsageIfCurrent(storage.TokenUsageWrite{
		JobID: job.ID, SessionID: usage.ThreadID,
		ExpectedTokenUsage: job.TokenUsage, ExpectedStartedAt: job.StartedAtRaw,
		TokenUsageJSON: tokens.ToJSON(usage),
	})
	if err != nil {
		log.Printf("[%s] save goal review usage: %v", workerID, err)
	}
	if updated {
		wp.invalidateBudgetSpend()
	}
}
