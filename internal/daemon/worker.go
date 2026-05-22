package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
	"go.kenn.io/roborev/internal/worktree"
)

// WorkerPool manages a pool of review workers
type WorkerPool struct {
	db          *storage.DB
	cfgGetter   ConfigGetter
	broadcaster Broadcaster
	errorLog    *ErrorLog
	activityLog *ActivityLog

	numWorkers    int
	activeWorkers atomic.Int32
	stopCh        chan struct{}
	readyCh       chan struct{} // closed after wg.Add in Start
	startOnce     sync.Once
	stopOnce      sync.Once
	wg            sync.WaitGroup

	// Track running jobs for cancellation
	runningJobs    map[int64]context.CancelFunc
	pendingCancels map[int64]bool // Jobs canceled before registered
	runningJobsMu  sync.Mutex

	// Agent cooldowns for quota exhaustion
	agentCooldowns   map[string]time.Time // agent name -> expiry
	agentCooldownsMu sync.RWMutex

	// classify is the rate-limit/quota classifier. Defaults to
	// agent.ClassifyLimit; tests substitute a stub by setting this
	// field directly after construction (test-only access).
	classify agent.LimitClassifier

	// Output capture for tail command
	outputBuffers *OutputBuffer

	// Test hooks for deterministic synchronization (nil in production)
	testHookAfterSecondCheck    func() // Called after second runningJobs check, before second DB lookup
	testHookCooldownLockUpgrade func() // Called between RUnlock and Lock in isAgentCoolingDown
}

// NewWorkerPool creates a new worker pool
func NewWorkerPool(db *storage.DB, cfgGetter ConfigGetter, numWorkers int, broadcaster Broadcaster, errorLog *ErrorLog, activityLog *ActivityLog) *WorkerPool {
	return &WorkerPool{
		db:             db,
		cfgGetter:      cfgGetter,
		broadcaster:    broadcaster,
		errorLog:       errorLog,
		activityLog:    activityLog,
		numWorkers:     numWorkers,
		stopCh:         make(chan struct{}),
		readyCh:        make(chan struct{}),
		runningJobs:    make(map[int64]context.CancelFunc),
		pendingCancels: make(map[int64]bool),
		agentCooldowns: make(map[string]time.Time),
		outputBuffers:  NewOutputBuffer(512*1024, 4*1024*1024), // 512KB/job, 4MB total
		classify:       agent.ClassifyLimit,
	}
}

// Start begins the worker pool. Safe to call multiple times;
// only the first call spawns workers.
func (wp *WorkerPool) Start() {
	wp.startOnce.Do(func() {
		log.Printf(
			"Starting worker pool with %d workers",
			wp.numWorkers,
		)
		wp.wg.Add(wp.numWorkers)
		close(wp.readyCh)
		for i := 0; i < wp.numWorkers; i++ {
			go wp.worker(i)
		}
	})
}

// Stop gracefully shuts down the worker pool. Safe to call
// multiple times; only the first call performs shutdown.
func (wp *WorkerPool) Stop() {
	wp.stopOnce.Do(func() {
		log.Println("Stopping worker pool...")
		close(wp.stopCh)
		// Wait for Start to finish wg.Add before calling Wait.
		// If Start was never called, readyCh stays open but
		// stopCh is closed, so any late workers exit immediately.
		select {
		case <-wp.readyCh:
			wp.wg.Wait()
		default:
		}
		log.Println("Worker pool stopped")
	})
}

// ActiveWorkers returns the number of currently active workers
func (wp *WorkerPool) ActiveWorkers() int {
	return int(wp.activeWorkers.Load())
}

// MaxWorkers returns the total number of workers in the pool
func (wp *WorkerPool) MaxWorkers() int {
	return wp.numWorkers
}

// GetJobOutput returns the current output lines for a job.
func (wp *WorkerPool) GetJobOutput(jobID int64) []OutputLine {
	return wp.outputBuffers.GetLines(jobID)
}

// SubscribeJobOutput returns initial lines and a channel for new output.
// Call cancel when done to unsubscribe.
func (wp *WorkerPool) SubscribeJobOutput(jobID int64) ([]OutputLine, <-chan OutputLine, func()) {
	return wp.outputBuffers.Subscribe(jobID)
}

// HasJobOutput returns true if there's active output capture for a job.
func (wp *WorkerPool) HasJobOutput(jobID int64) bool {
	return wp.outputBuffers.IsActive(jobID)
}

// CancelJob cancels a running job by its ID, killing the subprocess.
// Returns true if the job was canceled or marked for pending cancellation.
// Returns false only if the job doesn't exist or isn't in a cancellable state.
func (wp *WorkerPool) CancelJob(jobID int64) bool {
	wp.runningJobsMu.Lock()
	cancel, ok := wp.runningJobs[jobID]
	if ok {
		wp.runningJobsMu.Unlock()
		log.Printf("Canceling job %d", jobID)
		cancel()
		return true
	}
	wp.runningJobsMu.Unlock()

	// Job not registered yet - check if it's a valid job before marking pending
	// This prevents unbounded growth of pendingCancels for invalid/finished job IDs
	// Note: we release the lock before the DB call to avoid blocking other operations
	job, err := wp.db.GetJobByID(jobID)
	if err != nil {
		// DB error - but job may have registered while we were trying to read
		// Re-check runningJobs before giving up
		wp.runningJobsMu.Lock()
		if cancel, ok := wp.runningJobs[jobID]; ok {
			wp.runningJobsMu.Unlock()
			log.Printf("Canceling job %d (registered during failed DB check)", jobID)
			cancel()
			return true
		}
		wp.runningJobsMu.Unlock()
		return false
	}

	// Accept jobs that are queued, running, OR canceled-but-claimed (race condition case)
	// When db.CancelJob is called before workerPool.CancelJob, the status becomes 'canceled'
	// but the worker may not have registered yet. We detect this via WorkerID being set.
	if !wp.isJobCancellable(job) {
		return false
	}

	// Re-lock and check if job was registered while we were checking DB
	wp.runningJobsMu.Lock()
	if cancel, ok := wp.runningJobs[jobID]; ok {
		wp.runningJobsMu.Unlock()
		log.Printf("Canceling job %d (registered during DB check)", jobID)
		cancel()
		return true
	}
	wp.runningJobsMu.Unlock()

	// Test hook: allows tests to register job between second check and final check
	if wp.testHookAfterSecondCheck != nil {
		wp.testHookAfterSecondCheck()
	}

	// Re-verify job is still cancellable before adding to pendingCancels
	// The job may have registered and finished during our DB lookup window
	// Do this outside the lock to avoid blocking other operations
	job, err = wp.db.GetJobByID(jobID)
	if err != nil || !wp.isJobCancellable(job) {
		// Job finished or became non-cancellable - don't add stale entry
		return false
	}

	// Final lock acquisition to set pendingCancels
	wp.runningJobsMu.Lock()

	// Final check if job registered while we did the second DB lookup
	if cancel, ok := wp.runningJobs[jobID]; ok {
		wp.runningJobsMu.Unlock()
		log.Printf("Canceling job %d (registered during second DB check)", jobID)
		cancel()
		return true
	}

	// Mark for pending cancellation
	wp.pendingCancels[jobID] = true
	wp.runningJobsMu.Unlock()
	log.Printf("Job %d not yet registered, marking for pending cancellation", jobID)
	return true
}

// isJobCancellable returns true if the job is in a state that can be canceled
func (wp *WorkerPool) isJobCancellable(job *storage.ReviewJob) bool {
	return job.Status == storage.JobStatusQueued ||
		job.Status == storage.JobStatusRunning ||
		(job.Status == storage.JobStatusCanceled && job.WorkerID != "")
}

// registerRunningJob tracks a running job for potential cancellation.
// If the job was already marked for cancellation (race condition), it
// immediately cancels it.
func (wp *WorkerPool) registerRunningJob(jobID int64, cancel context.CancelFunc) {
	wp.runningJobsMu.Lock()
	wp.runningJobs[jobID] = cancel

	// Check if this job was canceled before we registered it
	if wp.pendingCancels[jobID] {
		delete(wp.pendingCancels, jobID)
		wp.runningJobsMu.Unlock()
		log.Printf("Job %d was pending cancellation, canceling now", jobID)
		cancel()
		return
	}
	wp.runningJobsMu.Unlock()
}

// IsJobPendingCancel reports whether a job is in the pendingCancels set.
// This is intended for use in tests.
func (wp *WorkerPool) IsJobPendingCancel(jobID int64) bool {
	wp.runningJobsMu.Lock()
	defer wp.runningJobsMu.Unlock()
	return wp.pendingCancels[jobID]
}

// unregisterRunningJob removes a job from the running jobs map
func (wp *WorkerPool) unregisterRunningJob(jobID int64) {
	wp.runningJobsMu.Lock()
	delete(wp.runningJobs, jobID)
	delete(wp.pendingCancels, jobID) // Clean up any stale pending cancel
	wp.runningJobsMu.Unlock()
}

func (wp *WorkerPool) worker(id int) {
	defer wp.wg.Done()
	workerID := fmt.Sprintf("worker-%d", id)

	log.Printf("[%s] Started", workerID)

	for {
		select {
		case <-wp.stopCh:
			log.Printf("[%s] Shutting down", workerID)
			return
		default:
		}

		// Try to claim a job
		job, err := wp.db.ClaimJob(workerID)
		if err != nil {
			log.Printf("[%s] Error claiming job: %v", workerID, err)
			if wp.errorLog != nil {
				wp.errorLog.LogError("worker", fmt.Sprintf("claim job: %v", err), 0)
			}
			select {
			case <-wp.stopCh:
				log.Printf("[%s] Shutting down", workerID)
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		if job == nil {
			// No jobs available, wait and retry
			select {
			case <-wp.stopCh:
				log.Printf("[%s] Shutting down", workerID)
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// Process the job
		wp.activeWorkers.Add(1)
		wp.processJob(workerID, job)
		wp.activeWorkers.Add(-1)
	}
}

// maxRetries is the number of retry attempts allowed after initial failure.
// With maxRetries=3, a job can run up to 4 times total (1 initial + 3 retries).
const maxRetries = 3

// reviewTypeTag returns a display prefix for non-default review types
// (e.g. "security "). Returns "" for the default review type to avoid
// redundant "review review" in log lines.
func reviewTypeTag(rt string) string {
	if config.IsDefaultReviewType(rt) {
		return ""
	}
	return rt + " "
}

func (wp *WorkerPool) processJob(workerID string, job *storage.ReviewJob) {
	rtTag := reviewTypeTag(job.ReviewType)

	log.Printf("[%s] Processing job %d %s %sreview/%s ref=%s",
		workerID, job.ID, job.RepoName,
		rtTag, job.Agent, gitpkg.ShortRef(job.GitRef))
	jobStart := time.Now()

	if wp.activityLog != nil {
		wp.activityLog.Log(
			"job.started", "worker",
			fmt.Sprintf("job %d started by %s", job.ID, workerID),
			map[string]string{
				"job_id": fmt.Sprintf("%d", job.ID),
				"worker": workerID,
				"agent":  job.Agent,
				"ref":    job.GitRef,
			},
		)
	}

	// Snapshot config once to ensure consistent settings throughout the job.
	// This prevents mixed settings if config reloads mid-job.
	cfg := wp.cfgGetter.Config()

	// Get timeout from config (per-repo or global, default 30 minutes)
	timeoutMinutes := config.ResolveJobTimeout(job.RepoPath, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMinutes)*time.Minute)
	defer cancel()

	// Register for cancellation tracking
	wp.registerRunningJob(job.ID, cancel)
	defer wp.unregisterRunningJob(job.ID)

	// Skip immediately if the agent is in quota cooldown.
	// Resolve alias so "claude" checks cooldown for "claude-code".
	canonicalAgent := agent.CanonicalName(job.Agent)
	if wp.isAgentCoolingDown(canonicalAgent) {
		log.Printf("[%s] Agent %s in cooldown, skipping job %d",
			workerID, canonicalAgent, job.ID)
		wp.failoverOrFail(workerID, job, canonicalAgent,
			fmt.Sprintf("agent %s quota cooldown active", canonicalAgent))
		return
	}

	// Classify jobs route through their own handler — no prompt building,
	// no review path; the row gets converted in place.
	if job.JobType == storage.JobTypeClassify {
		wp.processClassifyJob(ctx, workerID, job)
		return
	}

	// Resolve effective repo path: use worktree if available, still exists,
	// and is a valid git checkout for the same repository.
	effectiveRepoPath := job.RepoPath
	if job.WorktreePath != "" {
		if !gitpkg.ValidateWorktreeForRepo(job.WorktreePath, job.RepoPath) {
			log.Printf("[%s] Worktree %s invalid or gone for job %d, using main repo",
				workerID, job.WorktreePath, job.ID)
		} else {
			effectiveRepoPath = job.WorktreePath
		}
	}

	// Build the prompt (or use pre-stored prompt for task/compact jobs).
	// Create a per-job builder with the snapshotted config so exclude
	// patterns are resolved consistently.
	pb := prompt.NewBuilderWithConfig(wp.db, cfg).ForRepo(effectiveRepoPath, job.RepoID)
	if err := pb.CleanupStaleSnapshots(prompt.DefaultStaleSnapshotAge); err != nil {
		log.Printf("[%s] Warning: cleanup stale snapshots for job %d: %v", workerID, job.ID, err)
	}
	var reviewPrompt string
	var promptToPersist string
	storedPromptValue := job.Prompt
	var err error
	if job.PromptPrebuilt && storedPromptValue != "" {
		// CI-enqueued review with prebuilt prompt (includes PR
		// discussion context and system prompt). Use as-is so the
		// discussion context survives retries and failover.
		reviewPrompt = storedPromptValue
		promptToPersist = storedPromptValue
		var cleanup func()
		excludes := config.ResolveExcludePatterns(
			effectiveRepoPath, cfg, job.ReviewType,
		)
		reviewPrompt, cleanup, err = preparePrebuiltPrompt(
			effectiveRepoPath, job, reviewPrompt, excludes,
		)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			log.Printf("[%s] Error preparing prebuilt prompt: %v", workerID, err)
			wp.failOrRetry(workerID, job, job.Agent, fmt.Sprintf("prepare prebuilt prompt: %v", err))
			return
		}
	} else if job.UsesStoredPrompt() && job.Prompt != "" {
		// Prompt-native job (task, compact) — prepend agent-specific preamble
		preamble := prompt.GetSystemPrompt(job.Agent, "run")
		if preamble != "" {
			reviewPrompt = preamble + "\n" + job.Prompt
		} else {
			reviewPrompt = job.Prompt
		}
		promptToPersist = job.Prompt
	} else if job.UsesStoredPrompt() {
		// Prompt-native job (task/compact) with missing prompt — likely a
		// daemon version mismatch or storage issue. Fail clearly instead
		// of trying to build a prompt from a non-git label.
		err = fmt.Errorf("%s job %d has no stored prompt (git_ref=%q); restart the daemon with 'roborev daemon restart'", job.JobType, job.ID, job.GitRef)
	} else {
		// Resolve effective min-severity: job-level override wins, then cascade.
		minSev := job.MinSeverity
		if minSev == "" {
			resolved, resErr := config.ResolveReviewMinSeverity("", effectiveRepoPath, cfg)
			if resErr != nil {
				log.Printf("[%s] Error resolving min-severity: %v", workerID, resErr)
				wp.failOrRetry(workerID, job, job.Agent, fmt.Sprintf("resolve min-severity: %v", resErr))
				return
			}
			minSev = resolved
		}

		if job.DiffContent != nil {
			// Dirty job - use pre-captured diff
			dirtyResult, dirtyErr := pb.BuildDirtyWithSnapshot(*job.DiffContent, cfg.ReviewContextCount, job.Agent, job.ReviewType, minSev)
			if dirtyResult.Cleanup != nil {
				defer dirtyResult.Cleanup()
			}
			reviewPrompt = dirtyResult.Prompt
			err = dirtyErr
		} else {
			// Normal job - build prompt from git ref, writing a diff
			// snapshot file when the diff is too large to inline.
			excludes := config.ResolveExcludePatterns(
				effectiveRepoPath, cfg, job.ReviewType,
			)
			snapResult, snapErr := pb.BuildWithSnapshot(
				job.GitRef, cfg.ReviewContextCount, job.Agent,
				job.ReviewType, minSev, excludes,
			)
			if snapResult.Cleanup != nil {
				defer snapResult.Cleanup()
			}
			reviewPrompt = snapResult.Prompt
			err = snapErr
		}
	}
	if err != nil {
		log.Printf("[%s] Error building prompt: %v", workerID, err)
		wp.failOrRetry(workerID, job, job.Agent, fmt.Sprintf("build prompt: %v", err))
		return
	}
	if promptToPersist == "" {
		promptToPersist = reviewPrompt
	}

	// Save the prompt so it can be viewed while job is running
	if err := wp.db.SaveJobPrompt(job.ID, promptToPersist); err != nil {
		log.Printf("[%s] Error saving prompt: %v", workerID, err)
	}

	// Get the agent (falls back to available agent if preferred not installed)
	baseAgent, err := agent.GetAvailableWithConfig(job.RepoPath, job.Agent, cfg)
	if err != nil {
		log.Printf("[%s] Error getting agent: %v", workerID, err)
		wp.failOrRetryAgent(workerID, job, job.Agent, fmt.Sprintf("get agent: %v", err))
		return
	}

	// Use reasoning level from job (defaults to thorough for legacy rows)
	// Normalize legacy mixed-case/whitespace values (e.g., "FAST", "High") before parsing
	reasoning := strings.ToLower(strings.TrimSpace(job.Reasoning))
	if reasoning == "" {
		reasoning = "thorough"
	}
	reasoningLevel := agent.ParseReasoningLevel(reasoning)
	// Disable agentic mode when the prompt contains external data
	// (PR discussion) to prevent prompt-injection from influencing
	// tool-capable agents. CI reviews are always non-agentic, but
	// this guard defends against future callers setting the flag.
	agentic := job.Agentic
	if job.PromptPrebuilt {
		agentic = false
	}
	a := applyCodexReviewSettings(
		baseAgent.WithReasoning(reasoningLevel).WithAgentic(agentic).WithModel(job.Model),
		job,
		cfg,
	)
	if job.SessionID != "" {
		if !agent.IsValidResumeSessionID(job.SessionID) {
			log.Printf("[%s] Ignoring invalid session_id for job %d", workerID, job.ID)
		} else if sa, ok := a.(agent.SessionAgent); ok {
			a = sa.WithSessionID(job.SessionID)
		}
	}

	// Apply provider if set and agent supports it (e.g. pi agent)
	if job.Provider != "" {
		if pa, ok := a.(*agent.PiAgent); ok {
			a = pa.WithProvider(job.Provider)
		}
	}

	// Use the actual agent name (may differ from requested if fallback occurred)
	agentName := a.Name()
	if agentName != job.Agent {
		log.Printf("[%s] Agent %s not available, using %s", workerID, job.Agent, agentName)
	}

	// Store the actual command line so the TUI displays what the
	// daemon executes, not a client-side reconstruction.
	if err := wp.db.SaveJobCommandLine(job.ID, a.CommandLine()); err != nil {
		log.Printf("[%s] Error saving command line: %v", workerID, err)
	}

	// Enforce the final submission size after all prompt transformations.
	// Oversized prompts are deterministic and should never be sent to any
	// agent just to discover a context-window failure.
	maxPromptSize := config.ResolveMaxPromptSize(effectiveRepoPath, cfg)
	if maxPromptSize > 0 && len(reviewPrompt) > maxPromptSize {
		wp.failoverOrFailNonRetryableAgent(
			workerID, job, agentName,
			fmt.Sprintf("prompt exceeds size limit before agent submission: prompt is %d bytes, limit is %d bytes; use a backup agent that can read snapshot diff files or review a smaller range", len(reviewPrompt), maxPromptSize),
		)
		return
	}

	// Use the effective worktree path for events (empty when worktree is gone or not a worktree job).
	eventWorktreePath := ""
	if effectiveRepoPath != job.RepoPath {
		eventWorktreePath = effectiveRepoPath
	}

	// Broadcast started event
	wp.broadcaster.Broadcast(Event{
		Type:         "review.started",
		TS:           time.Now(),
		JobID:        job.ID,
		Repo:         job.RepoPath,
		RepoName:     job.RepoName,
		SHA:          job.GitRef,
		Agent:        agentName,
		WorktreePath: eventWorktreePath,
	})

	// Create output writer for tail command
	normalizer := GetNormalizer(agentName)
	outputWriter := wp.outputBuffers.Writer(job.ID, normalizer)
	defer func() {
		outputWriter.Flush()
		wp.outputBuffers.CloseJob(job.ID)
	}()

	// Tee raw agent output to a per-job log file on disk. The writer retries
	// transient filesystem failures so resource pressure does not permanently
	// disable logging for the rest of the job.
	jobLog := newJobLogWriter(job.ID)
	defer func() {
		if err := jobLog.Close(); err != nil {
			log.Printf("[%s] Warning: close job log for job %d: %v", workerID, job.ID, err)
		}
	}()
	agentOutput := io.MultiWriter(jobLog, outputWriter)
	sessionWriter := agent.NewSessionCaptureWriter(agentOutput, func(sessionID string) {
		if err := wp.db.SaveJobSessionID(job.ID, workerID, sessionID); err != nil {
			log.Printf("[%s] Error saving session ID for job %d: %v", workerID, job.ID, err)
		}
	})
	agentOutput = sessionWriter

	// For fix jobs, create an isolated worktree to run the agent in.
	// The agent modifies files in the worktree; afterwards we capture the diff as a patch.
	reviewRepoPath := effectiveRepoPath
	var fixWorktree *worktree.Worktree
	if job.IsFixJob() {
		wt, wtErr := worktree.Create(job.RepoPath, job.GitRef)
		if wtErr != nil {
			log.Printf("[%s] Error creating worktree for fix job %d: %v", workerID, job.ID, wtErr)
			wp.failOrRetry(workerID, job, agentName, fmt.Sprintf("create worktree: %v", wtErr))
			return
		}
		defer wt.Close()
		fixWorktree = wt
		reviewRepoPath = wt.Dir
		log.Printf("[%s] Fix job %d: running agent in worktree %s", workerID, job.ID, wt.Dir)
	}

	// Run the review
	log.Printf("[%s] Running %s %sreview (job %d)...",
		workerID, agentName, rtTag, job.ID)
	output, err := a.Review(ctx, reviewRepoPath, job.GitRef, reviewPrompt, agentOutput)
	sessionWriter.Flush()
	if sessionID := sessionWriter.SessionID(); sessionID != "" {
		if saveErr := wp.db.SaveJobSessionID(job.ID, workerID, sessionID); saveErr != nil {
			log.Printf("[%s] Error persisting session ID for job %d: %v", workerID, job.ID, saveErr)
		}
	}
	if err != nil {
		// Check if this was a cancellation
		if ctx.Err() == context.Canceled {
			log.Printf("[%s] Job %d was canceled", workerID, job.ID)
			// Broadcast cancellation event
			wp.broadcaster.Broadcast(Event{
				Type:         "review.canceled",
				TS:           time.Now(),
				JobID:        job.ID,
				Repo:         job.RepoPath,
				RepoName:     job.RepoName,
				SHA:          job.GitRef,
				Agent:        agentName,
				WorktreePath: eventWorktreePath,
			})
			return // Job already marked as canceled in DB, nothing more to do
		}
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			timeoutErr := fmt.Sprintf(
				"agent timeout after %s",
				(time.Duration(timeoutMinutes) * time.Minute).Round(time.Second),
			)
			log.Printf("[%s] Job %d timed out: %v", workerID, job.ID, err)
			wp.failOrRetryAgent(workerID, job, agentName, timeoutErr)
			return
		}
		log.Printf("[%s] Agent error on job %d: %v",
			workerID, job.ID, err)
		wp.failOrRetryAgent(workerID, job, agentName, fmt.Sprintf("agent: %v", err))
		return
	}

	// For fix jobs, capture the patch from the worktree. Patch capture
	// failures are fatal — a fix job without a patch is useless.
	var fixPatch string
	if job.IsFixJob() {
		var patchErr error
		fixPatch, patchErr = fixWorktree.CapturePatch()
		if patchErr != nil {
			log.Printf("[%s] Fix job %d: patch capture failed: %v", workerID, job.ID, patchErr)
			wp.failOrRetry(workerID, job, agentName, fmt.Sprintf("patch capture: %v", patchErr))
			return
		}
		if fixPatch == "" {
			log.Printf("[%s] Fix job %d: agent produced no file changes", workerID, job.ID)
			wp.failOrRetry(workerID, job, agentName, "agent produced no file changes")
			return
		}
		log.Printf("[%s] Fix job %d: captured patch (%d bytes)", workerID, job.ID, len(fixPatch))
	}

	// For compact jobs, validate raw agent output before storing.
	// Invalid output (empty, error patterns) should fail the job,
	// not produce a "done" review that misleads --wait callers.
	if job.JobType == "compact" && !IsValidCompactOutput(output) {
		log.Printf("[%s] Compact job %d produced invalid output, failing", workerID, job.ID)
		wp.failOrRetryAgent(workerID, job, agentName, "compact output invalid (empty or error)")
		return
	}

	// Store the result (use actual agent name, not requested).
	// CompleteJob/CompleteFixJob is a no-op (returns nil) if the job was
	// canceled between agent finish and now.
	if job.IsFixJob() {
		if err := wp.db.CompleteFixJob(job.ID, agentName, reviewPrompt, output, fixPatch); err != nil {
			log.Printf("[%s] Error storing fix review: %v", workerID, err)
			return
		}
	} else if err := wp.db.CompleteJob(job.ID, agentName, reviewPrompt, output); err != nil {
		log.Printf("[%s] Error storing review: %v", workerID, err)
		return
	}

	// Verify the job actually completed (not silently skipped due to
	// cancel race). CompleteJob/CompleteFixJob no-ops when status !=
	// running, so a job canceled between agent finish and DB update
	// must not broadcast review.completed or the batch counters will
	// over-count successes.
	{
		j, err := wp.db.GetJobByID(job.ID)
		if err != nil {
			log.Printf("[%s] Job %d: failed to verify status: %v", workerID, job.ID, err)
		} else if j.Status != storage.JobStatusDone {
			log.Printf("[%s] Job %d not completed (status=%s), skipping broadcast", workerID, job.ID, j.Status)
			return
		}
	}

	// For compact jobs, mark source jobs as closed now that we've
	// confirmed the compact job completed.
	if job.JobType == "compact" {
		if err := wp.markCompactSourceJobs(workerID, job.ID); err != nil {
			log.Printf("[%s] Warning: failed to mark compact source jobs for job %d: %v", workerID, job.ID, err)
		}
	}

	// Auto-close passing reviews when configured.
	if job.IsReviewJob() && storage.ParseVerdict(output) == "P" {
		cfg := wp.cfgGetter.Config()
		if config.ResolveAutoClosePassingReviews(job.RepoPath, cfg) {
			if err := wp.db.MarkReviewClosedByJobID(job.ID, true); err != nil {
				log.Printf("[%s] Warning: auto-close passing review for job %d: %v", workerID, job.ID, err)
			}
		}
	}

	// Fetch token usage from agentsview (best-effort).
	// Only collect for fresh sessions (where we captured a new session ID).
	// Resumed sessions report cumulative totals across all turns, which
	// would overcount if assigned to a single job.
	capturedSession := ""
	if sessionWriter != nil {
		capturedSession = sessionWriter.SessionID()
	}
	wasResumed := job.SessionID != "" && capturedSession == job.SessionID
	if capturedSession != "" && !wasResumed {
		usage, tokenErr := tokens.FetchForSession(
			context.Background(), capturedSession,
		)
		if tokenErr != nil {
			log.Printf("[%s] Warning: fetch token usage for job %d: %v",
				workerID, job.ID, tokenErr)
		} else if usage != nil {
			j := tokens.ToJSON(usage)
			if err := wp.db.SaveJobTokenUsage(job.ID, j); err != nil {
				log.Printf("[%s] Warning: save token usage for job %d: %v",
					workerID, job.ID, err)
			}
		}
	}

	log.Printf("[%s] Completed job %d %s %sreview/%s",
		workerID, job.ID, job.RepoName, rtTag, agentName)

	if wp.activityLog != nil {
		wp.activityLog.Log(
			"job.completed", "worker",
			fmt.Sprintf("job %d completed by %s", job.ID, workerID),
			map[string]string{
				"job_id":   fmt.Sprintf("%d", job.ID),
				"worker":   workerID,
				"agent":    agentName,
				"duration": time.Since(jobStart).Round(time.Second).String(),
			},
		)
	}

	// Broadcast completion event
	verdict := storage.ParseVerdict(output)
	wp.broadcaster.Broadcast(Event{
		Type:         "review.completed",
		TS:           time.Now(),
		JobID:        job.ID,
		Repo:         job.RepoPath,
		RepoName:     job.RepoName,
		SHA:          job.GitRef,
		Agent:        agentName,
		Verdict:      verdict,
		Findings:     output,
		WorktreePath: eventWorktreePath,
	})
}

func applyCodexReviewSettings(a agent.Agent, job *storage.ReviewJob, cfg *config.Config) agent.Agent {
	if !job.IsReviewJob() {
		return a
	}
	a = agent.WithCodexSkillsDisabled(
		a,
		config.ResolveDisableCodexReviewSkills(job.RepoPath, cfg),
	)
	a = agent.WithCodexUserConfigIgnored(
		a,
		config.ResolveIgnoreCodexReviewUserConfig(job.RepoPath, cfg),
	)
	return a
}

// failOrRetry attempts to retry the job, or marks it as failed if max retries reached.
// This is used for non-agent errors (e.g., prompt build failures) where switching agents won't help.
func (wp *WorkerPool) failOrRetry(workerID string, job *storage.ReviewJob, agentName string, errorMsg string) {
	wp.failOrRetryInner(workerID, job, agentName, errorMsg, false)
}

// failOrRetryAgent is like failOrRetry but allows failover to a backup agent
// when retries are exhausted. Used for agent-execution errors where switching
// agents may resolve the issue.
func (wp *WorkerPool) failOrRetryAgent(workerID string, job *storage.ReviewJob, agentName string, errorMsg string) {
	wp.failOrRetryInner(workerID, job, agentName, errorMsg, true)
}

func (wp *WorkerPool) failOrRetryInner(workerID string, job *storage.ReviewJob, agentName string, errorMsg string, agentError bool) {
	// Quota and session-limit errors skip retries entirely — cool down
	// the agent and attempt failover or fail. Behavior matches the
	// original isQuotaError branch; classification now lives in
	// internal/agent (ClassifyLimit) so the CLI fix loop can share it.
	if agentError {
		cls := wp.classify(agent.CanonicalName(agentName), errorMsg)
		switch cls.Kind {
		case agent.LimitKindQuota, agent.LimitKindSession:
			dur := defaultCooldown
			if cls.CooldownFor > 0 {
				dur = cls.CooldownFor
			}
			if !cls.ResetAt.IsZero() {
				if until := time.Until(cls.ResetAt); until > 0 {
					dur = until
				}
			}
			wp.cooldownAgent(agentName, time.Now().Add(dur))
			log.Printf("[%s] Agent %s quota exhausted, cooldown %v",
				workerID, agentName, dur)
			wp.failoverOrFail(workerID, job, agentName, errorMsg)
			return
		case agent.LimitKindNone:
			if errorMsg != "" {
				preview := strings.ReplaceAll(errorMsg, "\n", " ")
				preview = strings.ReplaceAll(preview, "\r", "")
				log.Printf("[%s] unclassified agent error from %s: %s",
					workerID, agentName, truncateRunes(preview, 200))
			}
			// fall through to context-window / retry handling
		case agent.LimitKindTransient:
			// fall through to retry handling
		}
	}
	if agentError && isContextWindowError(errorMsg) {
		wp.failoverOrFailNonRetryableAgent(workerID, job, agentName, errorMsg)
		return
	}

	retried, err := wp.db.RetryJob(job.ID, workerID, maxRetries)
	if err != nil {
		log.Printf("[%s] Error retrying job: %v", workerID, err)
		if updated, fErr := wp.db.FailJob(job.ID, workerID, errorMsg); fErr != nil {
			log.Printf("[%s] Error failing job %d: %v", workerID, job.ID, fErr)
		} else if updated {
			wp.broadcastFailed(job, agentName, errorMsg)
			if wp.errorLog != nil {
				wp.errorLog.LogError("worker", fmt.Sprintf("job %d failed: %s", job.ID, errorMsg), job.ID)
			}
			wp.logJobFailed(job.ID, workerID, agentName, errorMsg)
		}
		return
	}

	if retried {
		retryCount, _ := wp.db.GetJobRetryCount(job.ID)
		log.Printf("[%s] Job %d %s queued for retry (%d/%d)",
			workerID, job.ID, job.RepoName, retryCount, maxRetries)
	} else {
		// Retries exhausted -- attempt failover to backup agent if this is an agent error
		if agentError {
			backupAgent := wp.resolveBackupAgent(job)
			if backupAgent != "" && !wp.isAgentCoolingDown(backupAgent) {
				backupModel := wp.resolveBackupModel(job)
				failedOver, foErr := wp.db.FailoverJob(job.ID, workerID, backupAgent, backupModel)
				if foErr != nil {
					log.Printf("[%s] Error attempting failover for job %d: %v", workerID, job.ID, foErr)
				}
				if failedOver {
					log.Printf("[%s] Job %d failing over from %s to %s after %d retries: %s",
						workerID, job.ID, agentName, backupAgent, maxRetries, errorMsg)
					return
				}
			}
		}

		// No backup or failover failed -- mark as failed
		if updated, fErr := wp.db.FailJob(job.ID, workerID, errorMsg); fErr != nil {
			log.Printf("[%s] Error failing job %d: %v", workerID, job.ID, fErr)
		} else if updated {
			log.Printf("[%s] Job %d %s %sreview/%s failed after %d retries",
				workerID, job.ID, job.RepoName,
				reviewTypeTag(job.ReviewType), agentName,
				maxRetries)
			wp.broadcastFailed(job, agentName, errorMsg)
			if wp.errorLog != nil {
				wp.errorLog.LogError("worker", fmt.Sprintf("job %d failed after %d retries: %s", job.ID, maxRetries, errorMsg), job.ID)
			}
			wp.logJobFailed(job.ID, workerID, agentName, errorMsg)
		}
	}
}

func (wp *WorkerPool) failoverOrFailNonRetryableAgent(
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg string,
) {
	backupAgent := wp.resolveBackupAgent(job)
	if backupAgent != "" && !wp.isAgentCoolingDown(backupAgent) {
		backupModel := wp.resolveBackupModel(job)
		failedOver, err := wp.db.FailoverJob(job.ID, workerID, backupAgent, backupModel)
		if err != nil {
			log.Printf("[%s] Error attempting failover for job %d: %v",
				workerID, job.ID, err)
		}
		if failedOver {
			log.Printf("[%s] Job %d failing over from %s to %s (non-retryable): %s",
				workerID, job.ID, agentName, backupAgent, errorMsg)
			return
		}
	}

	if updated, err := wp.db.FailJob(job.ID, workerID, errorMsg); err != nil {
		log.Printf("[%s] Error failing job %d: %v", workerID, job.ID, err)
	} else if updated {
		log.Printf("[%s] Job %d %s %sreview/%s failed without retry: %s",
			workerID, job.ID, job.RepoName,
			reviewTypeTag(job.ReviewType), agentName, errorMsg)
		wp.broadcastFailed(job, agentName, errorMsg)
		if wp.errorLog != nil {
			wp.errorLog.LogError("worker", fmt.Sprintf("job %d failed without retry: %s", job.ID, errorMsg), job.ID)
		}
		wp.logJobFailed(job.ID, workerID, agentName, errorMsg)
	}
}

// failoverWorkflow returns the config workflow key for backup
// agent/model resolution. Fix jobs map to "fix"; security/design
// jobs use their ReviewType; everything else maps to "review".
func failoverWorkflow(job *storage.ReviewJob) string {
	if job.IsFixJob() {
		return "fix"
	}
	if !config.IsDefaultReviewType(job.ReviewType) {
		return job.ReviewType
	}
	return "review"
}

// resolveBackupAgent determines the backup agent for a job from config.
// Returns the canonicalized backup agent name, or "" if none is
// available or it's the same as the job's current agent.
func (wp *WorkerPool) resolveBackupAgent(job *storage.ReviewJob) string {
	cfg := wp.cfgGetter.Config()
	resolution, err := agent.ResolveWorkflowConfig(
		"", job.RepoPath, cfg, failoverWorkflow(job), "",
	)
	if err != nil {
		return ""
	}
	backup := resolution.BackupAgent
	if backup == "" {
		return ""
	}
	// Canonicalize: resolve alias, verify installed, skip if same as primary
	resolved, err := agent.Get(backup)
	if err != nil || !agent.IsAvailable(resolved.Name()) {
		return ""
	}
	if resolution.AgentMatches(resolved.Name(), job.Agent) {
		return ""
	}
	return resolved.Name()
}

// resolveBackupModel determines the backup model for a job from config.
// Returns the configured backup model, or "" if none is set.
func (wp *WorkerPool) resolveBackupModel(job *storage.ReviewJob) string {
	cfg := wp.cfgGetter.Config()
	resolution, err := agent.ResolveWorkflowConfig(
		"", job.RepoPath, cfg, failoverWorkflow(job), "",
	)
	if err != nil {
		return ""
	}
	return resolution.BackupModel()
}

// broadcastFailed sends a review.failed event for a job
func (wp *WorkerPool) broadcastFailed(job *storage.ReviewJob, agentName, errorMsg string) {
	wtPath := ""
	if job.WorktreePath != "" {
		if _, err := os.Stat(job.WorktreePath); err == nil {
			wtPath = job.WorktreePath
		}
	}
	wp.broadcaster.Broadcast(Event{
		Type:         "review.failed",
		TS:           time.Now(),
		JobID:        job.ID,
		Repo:         job.RepoPath,
		RepoName:     job.RepoName,
		SHA:          job.GitRef,
		Agent:        agentName,
		Error:        errorMsg,
		WorktreePath: wtPath,
	})
}

// defaultCooldown is the fallback duration when the error message doesn't
// contain a parseable "reset after" token.
const defaultCooldown = 30 * time.Minute

func isContextWindowError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	patterns := []string{
		"context window",
		"ran out of room",
		"context_length_exceeded",
		"maximum context length",
		"input is too long",
		"prompt is too long",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// cooldownAgent sets or extends the cooldown expiry for an agent.
// Only extends — never shortens an existing cooldown.
func (wp *WorkerPool) cooldownAgent(name string, until time.Time) {
	name = agent.CanonicalName(name)
	wp.agentCooldownsMu.Lock()
	defer wp.agentCooldownsMu.Unlock()
	if existing, ok := wp.agentCooldowns[name]; ok && existing.After(until) {
		return
	}
	wp.agentCooldowns[name] = until
}

// isAgentCoolingDown returns true if the agent is currently in a
// quota cooldown period. Expired entries are cleaned up eagerly.
func (wp *WorkerPool) isAgentCoolingDown(name string) bool {
	name = agent.CanonicalName(name)
	wp.agentCooldownsMu.RLock()
	expiry, ok := wp.agentCooldowns[name]
	if !ok {
		wp.agentCooldownsMu.RUnlock()
		return false
	}
	if time.Now().After(expiry) {
		wp.agentCooldownsMu.RUnlock()
		if wp.testHookCooldownLockUpgrade != nil {
			wp.testHookCooldownLockUpgrade()
		}
		// Upgrade to write lock and delete expired entry
		wp.agentCooldownsMu.Lock()
		// Re-check under write lock (may have been refreshed)
		exp, stillExists := wp.agentCooldowns[name]
		if stillExists && time.Now().After(exp) {
			delete(wp.agentCooldowns, name)
			wp.agentCooldownsMu.Unlock()
			return false
		}
		wp.agentCooldownsMu.Unlock()
		return stillExists
	}
	wp.agentCooldownsMu.RUnlock()
	return true
}

// failoverOrFail attempts failover to a backup agent for the job.
// If no backup is available, fails the job with a quota-prefixed error.
func (wp *WorkerPool) failoverOrFail(
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg string,
) {
	backupAgent := wp.resolveBackupAgent(job)
	if backupAgent != "" && !wp.isAgentCoolingDown(backupAgent) {
		backupModel := wp.resolveBackupModel(job)
		failedOver, err := wp.db.FailoverJob(
			job.ID, workerID, backupAgent, backupModel,
		)
		if err != nil {
			log.Printf("[%s] Error attempting failover for job %d: %v",
				workerID, job.ID, err)
		}
		if failedOver {
			log.Printf("[%s] Job %d failing over from %s to %s (quota): %s",
				workerID, job.ID, agentName, backupAgent, errorMsg)
			return
		}
	}

	// No backup or failover failed — fail with quota prefix
	quotaMsg := review.QuotaErrorPrefix + errorMsg
	if updated, err := wp.db.FailJob(job.ID, workerID, quotaMsg); err != nil {
		log.Printf("[%s] Error failing job %d: %v", workerID, job.ID, err)
	} else if updated {
		log.Printf("[%s] Job %d skipped (agent %s quota exhausted)",
			workerID, job.ID, agentName)
		wp.broadcastFailed(job, agentName, quotaMsg)
		if wp.errorLog != nil {
			wp.errorLog.LogError("worker",
				fmt.Sprintf("job %d skipped (quota): %s", job.ID, errorMsg),
				job.ID)
		}
		wp.logJobFailed(job.ID, workerID, agentName, quotaMsg)
	}
}

func preparePrebuiltPrompt(
	repoPath string, job *storage.ReviewJob, reviewPrompt string, excludes []string,
) (string, func(), error) {
	if !strings.Contains(reviewPrompt, prompt.DiffFilePathPlaceholder) {
		return reviewPrompt, nil, nil
	}
	builder := prompt.NewBuilder(nil).ForRepo(repoPath, job.RepoID)
	diffFile, cleanup, err := builder.WriteDiffSnapshot(job.GitRef, excludes)
	if err != nil {
		return "", nil, fmt.Errorf("prepare diff snapshot for prebuilt prompt: %w", err)
	}
	replacer := strings.NewReplacer(
		shellQuoteForPrompt(prompt.DiffFilePathPlaceholder),
		shellQuoteForPrompt(diffFile),
		prompt.DiffFilePathPlaceholder, diffFile,
	)
	return replacer.Replace(reviewPrompt), cleanup, nil
}

func shellQuoteForPrompt(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// logJobFailed logs a job failure to the activity log
func (wp *WorkerPool) logJobFailed(
	jobID int64, workerID, agentName, errorMsg string,
) {
	if wp.activityLog == nil {
		return
	}
	wp.activityLog.Log(
		"job.failed", "worker",
		fmt.Sprintf("job %d failed", jobID),
		map[string]string{
			"job_id": fmt.Sprintf("%d", jobID),
			"worker": workerID,
			"agent":  agentName,
			"error":  errorMsg,
		},
	)
}

// markCompactSourceJobs marks all source jobs as closed for a completed compact job
func (wp *WorkerPool) markCompactSourceJobs(workerID string, jobID int64) error {
	// Read metadata file, retrying briefly in case the CLI hasn't finished
	// writing it yet (the file is written after enqueue returns the job ID).
	var metadata *CompactMetadata
	var err error
	for attempt := range 3 {
		metadata, err = ReadCompactMetadata(jobID)
		if err == nil {
			break
		}
		if attempt < 2 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if err != nil {
		log.Printf("[%s] No compact metadata found for job %d after retries: %v", workerID, jobID, err)
		return nil
	}

	if len(metadata.SourceJobIDs) == 0 {
		log.Printf("[%s] No source jobs to mark for compact job %d", workerID, jobID)
		return nil
	}

	log.Printf("[%s] Marking %d source jobs as closed for compact job %d", workerID, len(metadata.SourceJobIDs), jobID)

	// Mark each source job as closed
	var failedIDs []int64
	for _, srcJobID := range metadata.SourceJobIDs {
		if err := wp.db.MarkReviewClosedByJobID(srcJobID, true); err != nil {
			log.Printf("[%s] Failed to mark job %d as closed: %v", workerID, srcJobID, err)
			failedIDs = append(failedIDs, srcJobID)
		}
	}

	successCount := len(metadata.SourceJobIDs) - len(failedIDs)
	if successCount > 0 {
		log.Printf("[%s] Marked %d/%d source jobs as closed", workerID, successCount, len(metadata.SourceJobIDs))
	}

	// Only delete metadata when all source jobs were marked.
	// On partial failure, keep metadata so a re-run can retry.
	if len(failedIDs) > 0 {
		log.Printf("[%s] Keeping compact metadata for job %d (%d jobs failed)", workerID, jobID, len(failedIDs))
		return nil
	}

	if err := DeleteCompactMetadata(jobID); err != nil {
		log.Printf("[%s] Failed to delete compact metadata for job %d: %v", workerID, jobID, err)
	} else {
		log.Printf("[%s] Cleaned up compact metadata for job %d", workerID, jobID)
	}

	return nil
}
