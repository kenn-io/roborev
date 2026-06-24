package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gitworktree "go.kenn.io/kit/git/worktree"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/backfill"
	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
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

	// tokenUsageFetcher looks up captured session usage. Nil uses the
	// configured tokens.FetchForSessionWithConfig path; tests substitute a
	// deterministic fetcher.
	tokenUsageFetcher func(context.Context, string) (*tokens.Usage, error)

	// Output capture for tail command
	outputBuffers *OutputBuffer

	// retryBackoff is the delay applied to requeued jobs before they
	// become claimable again. Stored on the job (via retry_not_before)
	// so any worker — not just the one that failed — honors it. Prevents
	// rapid concurrent agent startups racing on shared state (notably
	// opencode's sqlite WAL, where PRAGMA wal_checkpoint(PASSIVE) fails
	// when a prior crashed instance left the WAL mid-write).
	retryBackoff time.Duration

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
		retryBackoff:   2 * time.Second,
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

// resolveEffectiveRepoPath returns the non-CI checkout a job should run
// against: its worktree when set and still a valid checkout of the same repo,
// otherwise the main repo path.
func resolveEffectiveRepoPath(workerID string, job *storage.ReviewJob) string {
	if job.WorktreePath == "" {
		return job.RepoPath
	}
	if !gitpkg.ValidateWorktreeForRepo(job.WorktreePath, job.RepoPath) {
		log.Printf("[%s] Worktree %s invalid or gone for job %d, using main repo",
			workerID, job.WorktreePath, job.ID)
		return job.RepoPath
	}
	return job.WorktreePath
}

type preparedJobCheckout struct {
	// promptRepoPath is the trusted checkout used for prompt-side config,
	// excludes, max-prompt sizing, and snapshot_dir config.
	promptRepoPath string
	// agentRepoPath is the checkout used as the agent cwd.
	agentRepoPath string
	// snapshotTarget controls where oversized diff snapshot files are written.
	// CI writes them into the exact agent checkout while resolving snapshot_dir
	// from the trusted prompt checkout.
	snapshotTarget prompt.SnapshotTarget
	// eventWorktreePath is the caller-provided worktree path safe to expose
	// to event consumers and hooks. Internal CI checkouts must stay private.
	eventWorktreePath string
	cleanup           func()
}

func (wp *WorkerPool) prepareJobCheckout(
	ctx context.Context, workerID string, job *storage.ReviewJob,
) (preparedJobCheckout, error) {
	requiresCIWorktree, err := wp.jobRequiresCIExactCheckout(job)
	if err != nil {
		return preparedJobCheckout{}, err
	}
	if !requiresCIWorktree {
		repoPath := resolveEffectiveRepoPath(workerID, job)
		eventWorktreePath := ""
		if job.WorktreePath != "" && repoPath == job.WorktreePath {
			eventWorktreePath = job.WorktreePath
		}
		return preparedJobCheckout{
			promptRepoPath:    repoPath,
			agentRepoPath:     repoPath,
			eventWorktreePath: eventWorktreePath,
		}, nil
	}
	agentRepoPath, cleanup, err := wp.createCIExactCheckout(ctx, workerID, job)
	if err != nil {
		return preparedJobCheckout{}, err
	}
	return preparedJobCheckout{
		promptRepoPath: job.RepoPath,
		agentRepoPath:  agentRepoPath,
		snapshotTarget: prompt.SnapshotTarget{
			RepoPath:       agentRepoPath,
			ConfigRepoPath: job.RepoPath,
		},
		cleanup: cleanup,
	}, nil
}

// markAgentInvoked records that an agent is being invoked for this attempt. Call
// it only after all pre-agent gates pass, immediately before the agent runs, so
// a job that fails a gate is never counted as an agent run. This is the synced
// cost-eligibility signal (and stores the command line for TUI display); a
// failed write only under-reports cost, so it is logged, not fatal.
func (wp *WorkerPool) markAgentInvoked(workerID string, job *storage.ReviewJob, a agent.Agent) {
	if err := wp.db.MarkJobAgentInvoked(job.ID, workerID, a.CommandLine()); err != nil {
		log.Printf("[%s] Error marking agent invoked for job %d: %v", workerID, job.ID, err)
	}
}

func (wp *WorkerPool) jobRequiresCIExactCheckout(job *storage.ReviewJob) (bool, error) {
	if job == nil || job.PanelRunUUID == "" || job.IsFixJob() {
		return false, nil
	}
	if job.Source == storage.JobSourceCI {
		return true, nil
	}
	if wp.db == nil {
		return false, nil
	}
	if _, err := wp.db.GetCIPanelByRunUUID(job.PanelRunUUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("lookup CI panel run: %w", err)
	}
	return true, nil
}

func (wp *WorkerPool) createCIExactCheckout(
	ctx context.Context, workerID string, job *storage.ReviewJob,
) (string, func(), error) {
	headRef := strings.TrimSpace(headOf(job.GitRef))
	if headRef == "" {
		return "", nil, fmt.Errorf("CI job %d has empty checkout ref", job.ID)
	}
	parentDir := ciWorktreeRepoDir(job.RepoPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create CI worktree parent: %w", err)
	}
	unlock := lockGitMetadata(job.RepoPath)
	wt, createErr := gitworktree.Create(ctx, job.RepoPath, headRef, gitworktree.Options{
		ParentDir:      parentDir,
		Prefix:         fmt.Sprintf("%s%d-", ciWorktreePrefix, job.ID),
		InitSubmodules: true,
		PullLFS:        true,
	})
	if createErr == nil {
		createErr = writeCIWorktreeMarker(wt.Dir, job.RepoPath)
		if createErr != nil {
			_ = wt.Close(context.Background())
			createErr = fmt.Errorf("write CI worktree marker: %w", createErr)
		}
	}
	unlock()
	if createErr != nil {
		return "", nil, createErr
	}
	log.Printf("[%s] CI job %d: running agent in exact checkout %s (%s)",
		workerID, job.ID, wt.Dir, gitpkg.ShortRef(headRef))
	cleanup := func() {
		unlock := lockGitMetadata(job.RepoPath)
		defer unlock()
		if err := wt.Close(context.Background()); err != nil {
			log.Printf("[%s] Warning: remove CI worktree for job %d: %v",
				workerID, job.ID, err)
		}
	}
	return wt.Dir, cleanup, nil
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

		paused, err := wp.db.IsQueuePaused()
		if err != nil {
			log.Printf("[%s] Error checking queue pause state: %v", workerID, err)
			if wp.errorLog != nil {
				wp.errorLog.LogError("worker", fmt.Sprintf("check queue pause state: %v", err), 0)
			}
			select {
			case <-wp.stopCh:
				log.Printf("[%s] Shutting down", workerID)
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if paused {
			select {
			case <-wp.stopCh:
				log.Printf("[%s] Shutting down", workerID)
				return
			case <-time.After(2 * time.Second):
			}
			continue
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

	// Get timeout from config (per-repo or global, default 30 minutes), then
	// overlay any frozen panel-member timeout captured at enqueue time.
	timeoutMinutes := config.ResolveJobTimeout(job.RepoPath, cfg)
	timeoutDuration := resolveJobTimeoutDuration(job, timeoutMinutes)
	ctx, cancel := context.WithTimeout(context.Background(), timeoutDuration)
	defer cancel()

	// Register for cancellation tracking
	wp.registerRunningJob(job.ID, cancel)
	defer wp.unregisterRunningJob(job.ID)

	// Synthesis jobs route to their own handler before the cooldown gate: the
	// all-failed and passthrough branches call no agent, so a synthesis-agent
	// quota cooldown must not skip or fail them. Placing this after
	// registerRunningJob keeps synthesis jobs cancellable.
	if job.IsSynthesisJob() {
		wp.processSynthesisJob(ctx, workerID, job)
		return
	}

	// Skip immediately if the agent is in quota cooldown.
	// Resolve alias so "claude" checks cooldown for "claude-code".
	canonicalAgent := agent.CanonicalName(job.Agent)
	if wp.isAgentCoolingDown(canonicalAgent) {
		log.Printf("[%s] Agent %s in cooldown, skipping job %d",
			workerID, canonicalAgent, job.ID)
		wp.failCooldownOrFailover(workerID, job, canonicalAgent,
			fmt.Sprintf("agent %s quota cooldown active", canonicalAgent))
		return
	}

	// Classify jobs route through their own handler — no prompt building,
	// no review path; the row gets converted in place.
	if job.JobType == storage.JobTypeClassify {
		wp.processClassifyJob(ctx, workerID, job)
		return
	}

	// Resolve checkouts for this job. CI panel jobs run agents in a detached
	// worktree at the reviewed head; prompt-side config stays tied to the
	// trusted shared clone, while snapshots are written where the agent can read
	// them.
	checkout, err := wp.prepareJobCheckout(ctx, workerID, job)
	if checkout.cleanup != nil {
		defer checkout.cleanup()
	}
	if err != nil {
		log.Printf("[%s] Error preparing checkout: %v", workerID, err)
		wp.failOrRetry(workerID, job, job.Agent, fmt.Sprintf("prepare checkout: %v", err))
		return
	}

	// Build the prompt (or use pre-stored prompt for task/compact jobs).
	// Create a per-job builder with the snapshotted config so exclude
	// patterns are resolved consistently.
	pb := prompt.NewBuilderWithConfig(wp.db, cfg).WithContext(ctx).ForRepo(checkout.promptRepoPath, job.RepoID)
	// CI jobs normally carry a prebuilt prompt whose kata context was gated
	// on PR author trust by the poller. This rebuild fallback has no author
	// information, so it must not resolve kata refs from fork-controlled
	// commit messages (or leak backlog content) — skip kata context entirely.
	if !job.IsCIReview() {
		pb = pb.WithKataClient(kata.NewCLIClient(checkout.promptRepoPath))
	}
	if err := pb.CleanupStaleSnapshots(prompt.DefaultStaleSnapshotAge); err != nil {
		log.Printf("[%s] Warning: cleanup stale snapshots for job %d: %v", workerID, job.ID, err)
	}
	var reviewPrompt string
	var promptToPersist string
	storedPromptValue := job.Prompt
	if job.PromptPrebuilt && storedPromptValue != "" {
		// CI-enqueued review with prebuilt prompt (includes PR
		// discussion context and system prompt). Use as-is so the
		// discussion context survives retries and failover.
		reviewPrompt = storedPromptValue
		promptToPersist = storedPromptValue
		var cleanup func()
		excludes := config.ResolveExcludePatterns(
			ctx, checkout.promptRepoPath, cfg, job.ReviewType,
		)
		reviewPrompt, cleanup, err = preparePrebuiltPrompt(
			ctx, checkout.promptRepoPath, checkout.snapshotTarget, job, reviewPrompt, excludes,
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
			resolved, resErr := config.ResolveReviewMinSeverity("", checkout.promptRepoPath, cfg)
			if resErr != nil {
				log.Printf("[%s] Error resolving min-severity: %v", workerID, resErr)
				wp.failOrRetry(workerID, job, job.Agent, fmt.Sprintf("resolve min-severity: %v", resErr))
				return
			}
			minSev = resolved
		}

		if job.IsDirtyJob() {
			// Dirty job - use pre-captured diff
			diffContent := ""
			if job.DiffContent != nil {
				diffContent = *job.DiffContent
			}
			dirtyResult, dirtyErr := pb.BuildDirtyWithSnapshotTargetAndFiles(
				diffContent, job.DirtyFiles, cfg.ReviewContextCount, job.Agent, job.ReviewType, minSev,
				checkout.snapshotTarget,
			)
			if dirtyResult.Cleanup != nil {
				defer dirtyResult.Cleanup()
			}
			reviewPrompt = dirtyResult.Prompt
			err = dirtyErr
		} else {
			// Normal job - build prompt from git ref, writing a diff
			// snapshot file when the diff is too large to inline.
			excludes := config.ResolveExcludePatterns(
				ctx, checkout.promptRepoPath, cfg, job.ReviewType,
			)
			snapResult, snapErr := pb.BuildWithSnapshotTarget(
				job.GitRef, cfg.ReviewContextCount, job.Agent,
				job.ReviewType, minSev, excludes, checkout.snapshotTarget,
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
	// Panel members carry trusted reviewer instructions resolved at enqueue
	// time. Append them after every prompt path (snapshot/dirty/range) and
	// before promptToPersist defaults, so they persist and show in the view.
	reviewPrompt += memberInstructionSuffix(job)
	if promptToPersist == "" {
		promptToPersist = reviewPrompt
	}

	// Save the prompt so it can be viewed while job is running
	if err := wp.db.SaveJobPrompt(job.ID, promptToPersist); err != nil {
		log.Printf("[%s] Error saving prompt: %v", workerID, err)
	}

	// Get the configured job agent. Backup failover is handled explicitly by
	// failOrRetryAgent so jobs never silently run on the hardcoded fallback chain.
	baseAgent, err := agent.GetPreferredOrBackupWithConfig(job.RepoPath, job.Agent, cfg)
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

	// Enforce the final submission size after all prompt transformations.
	// Oversized prompts are deterministic and should never be sent to any
	// agent just to discover a context-window failure.
	maxPromptSize := config.ResolveMaxPromptSize(checkout.promptRepoPath, cfg)
	if maxPromptSize > 0 && len(reviewPrompt) > maxPromptSize {
		wp.failoverOrFailNonRetryableAgent(
			workerID, job, agentName,
			fmt.Sprintf("prompt exceeds size limit before agent submission: prompt is %d bytes, limit is %d bytes; use a backup agent that can read snapshot diff files or review a smaller range", len(reviewPrompt), maxPromptSize),
		)
		return
	}

	eventWorktreePath := checkout.eventWorktreePath

	// Broadcast started event
	wp.broadcaster.Broadcast(Event{
		Type:         "review.started",
		TS:           time.Now(),
		JobID:        job.ID,
		Repo:         job.RepoPath,
		RepoName:     job.RepoName,
		SHA:          job.GitRef,
		Branch:       job.HookBranch(),
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
	var jobLog *jobLogWriter
	if shouldAppendReviewJobLog(job) {
		jobLog = newAppendingJobLogWriter(job.ID)
	} else {
		jobLog = newJobLogWriter(job.ID)
	}
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
	reviewRepoPath := checkout.agentRepoPath
	var fixWorktree *gitworktree.Worktree
	if job.IsFixJob() {
		wt, wtErr := gitworktree.Create(ctx, job.RepoPath, job.GitRef, gitworktree.Options{
			Prefix:         "roborev-worktree-",
			InitSubmodules: true,
			PullLFS:        true,
		})
		if wtErr != nil {
			log.Printf("[%s] Error creating worktree for fix job %d: %v", workerID, job.ID, wtErr)
			wp.failOrRetry(workerID, job, agentName, fmt.Sprintf("create worktree: %v", wtErr))
			return
		}
		defer func() {
			if err := wt.Close(context.Background()); err != nil {
				log.Printf("[%s] Warning: remove worktree for fix job %d: %v", workerID, job.ID, err)
			}
		}()
		fixWorktree = wt
		reviewRepoPath = wt.Dir
		log.Printf("[%s] Fix job %d: running agent in worktree %s", workerID, job.ID, wt.Dir)
	}

	// Record that an agent is being invoked, now that all pre-agent gates
	// (prompt size, worktree creation) have passed.
	wp.markAgentInvoked(workerID, job, a)

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
				Branch:       job.HookBranch(),
				Agent:        agentName,
				WorktreePath: eventWorktreePath,
			})
			// Member canceled is terminal — release the panel synthesis.
			wp.releaseIfPanelMember(job)
			return // Job already marked as canceled in DB, nothing more to do
		}
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			timeoutErr := fmt.Sprintf(
				"agent timeout after %s",
				timeoutDuration.Round(time.Second),
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
		fixPatch, patchErr = fixWorktree.CapturePatch(ctx)
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

	wp.autoClosePassingReview(workerID, job, output)

	wp.captureTokenUsageForSession(context.Background(), workerID, job, sessionWriter.SessionID())

	// Member done — release the panel synthesis once all members are terminal.
	wp.releaseIfPanelMember(job)

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
		JobUUID:      job.UUID,
		Repo:         job.RepoPath,
		RepoName:     job.RepoName,
		SHA:          job.GitRef,
		Branch:       job.HookBranch(),
		Agent:        agentName,
		Verdict:      verdict,
		Findings:     output,
		WorktreePath: eventWorktreePath,
	})
}

func shouldAppendReviewJobLog(job *storage.ReviewJob) bool {
	return job.Source == "auto_design" && job.RetryCount == 0
}

func (wp *WorkerPool) autoClosePassingReview(workerID string, job *storage.ReviewJob, output string) {
	if !job.IsReviewJob() && !job.IsSynthesisJob() {
		return
	}
	if storage.ParseVerdict(output) != "P" {
		return
	}
	cfg := wp.cfgGetter.Config()
	if !config.ResolveAutoClosePassingReviews(job.RepoPath, cfg) {
		return
	}
	if err := wp.db.MarkReviewClosedByJobID(job.ID, true); err != nil {
		log.Printf("[%s] Warning: auto-close passing review for job %d: %v", workerID, job.ID, err)
	}
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

// finalErrorMsg tags the stored error with review.OutageErrorPrefix when an
// agent failure classifies as a transient provider outage or session cap so the
// CI batch layer can treat it as retryable rather than a genuine failure.
// Non-agent and non-transient errors are returned unchanged.
func (wp *WorkerPool) finalErrorMsg(agentName, errorMsg string, agentError bool) string {
	if !agentError {
		return errorMsg
	}
	switch wp.classify(agent.CanonicalName(agentName), errorMsg).Kind {
	case agent.LimitKindTransient, agent.LimitKindSession:
		return review.OutageError(errorMsg)
	default:
		return errorMsg
	}
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
			dur := wp.agentQuotaCooldown()
			if cls.CooldownFor > 0 && cls.CooldownFor < dur {
				dur = cls.CooldownFor
			}
			if !cls.ResetAt.IsZero() {
				if until := time.Until(cls.ResetAt); until > 0 && until < dur {
					dur = until
				}
			}
			wp.cooldownAgent(agentName, time.Now().Add(dur))
			log.Printf("[%s] Agent %s limit exhausted, cooldown %v",
				workerID, agentName, dur)
			prefix := review.QuotaErrorPrefix
			label := "quota"
			if cls.Kind == agent.LimitKindSession {
				prefix = review.OutageErrorPrefix
				label = "session limit"
			}
			wp.failoverOrFailWithPrefix(workerID, job, agentName, errorMsg, prefix, label)
			return
		case agent.LimitKindNone:
			if errorMsg != "" {
				log.Printf("[%s] unclassified agent error from %s: %s",
					workerID, agentName, logExcerpt(errorMsg))
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

	retried, err := wp.db.RetryJob(job.ID, workerID, maxRetries, wp.retryBackoff)
	if err != nil {
		log.Printf("[%s] Error retrying job: %v", workerID, err)
		if updated, fErr := wp.db.FailJob(job.ID, workerID, wp.finalErrorMsg(agentName, errorMsg, agentError)); fErr != nil {
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
		if updated, fErr := wp.db.FailJob(job.ID, workerID, wp.finalErrorMsg(agentName, errorMsg, agentError)); fErr != nil {
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

// resolveBackupAgent determines the backup agent for a job. An explicit
// per-job backup (job.BackupAgent) is preferred and returned canonicalized.
// Otherwise it falls back to the workflow config, returning the canonicalized
// backup agent name, or "" if none is available or it's the same as the job's
// current agent.
func (wp *WorkerPool) resolveBackupAgent(job *storage.ReviewJob) string {
	// An explicit per-job backup wins, for reliability: the enqueuer (e.g. a CI
	// panel synthesis) chose this failover deliberately. Canonicalize the alias
	// (e.g. "claude" -> "claude-code") via the registry; fall back to the raw
	// value if the name is unknown so the override is never silently dropped.
	// Gate on PRESENCE only (job.BackupAgent != "") so resolveBackupModel agrees
	// on whether the stored backup is in play. Note: agent.Get resolves from the
	// registry, not PATH, so this does not require local installation.
	if job.BackupAgent != "" {
		if resolved, err := agent.Get(job.BackupAgent); err == nil {
			return resolved.Name()
		}
		return job.BackupAgent
	}
	if job.JobType == storage.JobTypeSynthesis {
		return ""
	}
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
	// Canonicalize and verify availability using the config-aware path so
	// command overrides and configured ACP aliases participate in failover.
	resolved, err := agent.GetPreferredOrBackupWithConfig(job.RepoPath, backup, cfg)
	if err != nil {
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
	// Stored backup agent present => use the stored model, even if empty. Never
	// fall through to the workflow backup model, which is resolved for a
	// different agent (the user's F7 guardrail). Same presence gate as
	// resolveBackupAgent so the two never disagree.
	if job.BackupAgent != "" {
		return job.BackupModel
	}
	if job.JobType == storage.JobTypeSynthesis {
		return ""
	}
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
		JobUUID:      job.UUID,
		Repo:         job.RepoPath,
		RepoName:     job.RepoName,
		SHA:          job.GitRef,
		Branch:       job.HookBranch(),
		Agent:        agentName,
		Error:        errorMsg,
		WorktreePath: wtPath,
	})
	// broadcastFailed is the terminal-failure chokepoint (never reached on
	// retry/failover), so a member that finally fails releases its panel's
	// synthesis here. No-op for non-member and synthesis jobs (role gate).
	wp.releaseIfPanelMember(job)
}

// memberInstructionSuffix returns the trusted reviewer-instruction block to
// append to a panel member's review prompt. It returns "" for a non-member job,
// or when the member config is missing/unparseable or carries no instructions.
func memberInstructionSuffix(job *storage.ReviewJob) string {
	if job.PanelRole != storage.PanelRoleMember || job.PanelMemberConfigJSON == "" {
		return ""
	}
	var m config.ResolvedMember
	if json.Unmarshal([]byte(job.PanelMemberConfigJSON), &m) != nil || m.Instructions == "" {
		return ""
	}
	return fmt.Sprintf(
		"\n\n## Additional reviewer instructions (panel: %s / member: %s)\n%s\n",
		job.PanelName, job.PanelMemberName, m.Instructions,
	)
}

func resolveJobTimeoutDuration(job *storage.ReviewJob, defaultMinutes int) time.Duration {
	defaultDuration := time.Duration(defaultMinutes) * time.Minute
	if job.PanelRole != storage.PanelRoleMember || job.PanelMemberConfigJSON == "" {
		return defaultDuration
	}
	var m config.ResolvedMember
	if json.Unmarshal([]byte(job.PanelMemberConfigJSON), &m) != nil || m.Timeout == "" {
		return defaultDuration
	}
	d, err := time.ParseDuration(m.Timeout)
	if err != nil || d <= 0 {
		return defaultDuration
	}
	return d
}

// releaseIfPanelMember releases a panel run's blocked synthesis job when this
// member reaches a terminal state. MaybeReleasePanelSynthesis is idempotent and
// only releases once every member is terminal, so calling it on each member's
// terminal transition is safe; the Task 7 sweep is the backstop.
func (wp *WorkerPool) releaseIfPanelMember(job *storage.ReviewJob) {
	if job.PanelRole == storage.PanelRoleMember && job.PanelRunUUID != "" {
		if err := wp.db.MaybeReleasePanelSynthesis(job.PanelRunUUID); err != nil {
			log.Printf("panel %s: release synthesis: %v", job.PanelRunUUID, err)
		}
	}
}

func (wp *WorkerPool) captureTokenUsageForSession(
	ctx context.Context, workerID string, job *storage.ReviewJob, capturedSession string,
) {
	// Only fetch agentsview usage for fresh sessions (where we captured a new
	// session ID). Resumed-session agentsview totals are cumulative across
	// turns, but Codex job-log turn.completed usage belongs to this job's
	// stream and is safe to parse below.
	wasResumed := job.SessionID != "" && capturedSession == job.SessionID

	var usage *tokens.Usage
	logUsage, logErr := tokens.ParseCodexUsageFile(JobLogPath(job.ID))
	if logErr != nil {
		log.Printf("[%s] Warning: parse token usage from job log for job %d: %v",
			workerID, job.ID, logErr)
	} else if logUsage != nil {
		usage = logUsage
	}

	if capturedSession != "" && !wasResumed {
		fetcher := wp.tokenUsageFetcher
		if fetcher == nil {
			cfg := wp.cfgGetter.Config()
			fetcher = func(ctx context.Context, sessionID string) (*tokens.Usage, error) {
				return tokens.FetchForSessionWithConfig(
					ctx, sessionID,
					tokens.FetchConfig{
						Endpoint: cfg.Cost.Endpoint,
						Timeout:  cfg.Cost.ResolvedTimeout(),
					},
				)
			}
		}
		fetched, tokenErr := fetcher(ctx, capturedSession)
		if tokenErr != nil {
			log.Printf("[%s] Warning: fetch token usage for job %d: %v",
				workerID, job.ID, tokenErr)
		} else {
			usage = backfill.MergeTokenUsage(tokens.ToJSON(usage), fetched)
		}
	}

	if usage == nil {
		return
	}
	sessionID := capturedSession
	if sessionID == "" {
		sessionID = usage.ThreadID
	}
	if sessionID == "" {
		return
	}
	var err error
	if capturedSession != "" {
		err = wp.db.SaveJobTokenUsage(job.ID, sessionID, tokens.ToJSON(usage))
		if err == nil {
			err = wp.db.BackfillJobTokenUsage(job.ID, sessionID, tokens.ToJSON(usage))
		}
	} else {
		err = wp.db.BackfillJobTokenUsage(job.ID, sessionID, tokens.ToJSON(usage))
	}
	if err != nil {
		log.Printf("[%s] Warning: save token usage for job %d: %v",
			workerID, job.ID, err)
	}
}

func (wp *WorkerPool) agentQuotaCooldown() time.Duration {
	if wp == nil || wp.cfgGetter == nil {
		return config.DefaultAgentQuotaCooldown
	}
	return config.ResolveAgentQuotaCooldown(wp.cfgGetter.Config())
}

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

// cooldownAgent records the cooldown expiry for an agent, bounded by the
// current configured maximum. Later provider hints may shorten an existing
// cooldown; a subsequent quota error can extend it again, but never past config.
func (wp *WorkerPool) cooldownAgent(name string, until time.Time) {
	name = agent.CanonicalName(name)
	until = wp.clampAgentCooldownExpiry(until, time.Now())
	wp.agentCooldownsMu.Lock()
	defer wp.agentCooldownsMu.Unlock()
	wp.agentCooldowns[name] = until
}

// isAgentCoolingDown returns true if the agent is currently in a
// quota cooldown period. Expired entries are cleaned up eagerly.
func (wp *WorkerPool) isAgentCoolingDown(name string) bool {
	name = agent.CanonicalName(name)
	now := time.Now()
	wp.agentCooldownsMu.RLock()
	expiry, ok := wp.agentCooldowns[name]
	if !ok {
		wp.agentCooldownsMu.RUnlock()
		return false
	}
	if now.After(expiry) {
		wp.agentCooldownsMu.RUnlock()
		if wp.testHookCooldownLockUpgrade != nil {
			wp.testHookCooldownLockUpgrade()
		}
		wp.agentCooldownsMu.Lock()
		cooling := wp.clampActiveCooldownLocked(name)
		wp.agentCooldownsMu.Unlock()
		return cooling
	}
	clampedExpiry := wp.clampAgentCooldownExpiry(expiry, now)
	if clampedExpiry.Before(expiry) {
		wp.agentCooldownsMu.RUnlock()
		wp.agentCooldownsMu.Lock()
		cooling := wp.clampActiveCooldownLocked(name)
		wp.agentCooldownsMu.Unlock()
		return cooling
	}
	wp.agentCooldownsMu.RUnlock()
	return true
}

// clampActiveCooldownLocked rechecks and clamps a cooldown under the write lock.
// The caller must hold agentCooldownsMu for writing.
func (wp *WorkerPool) clampActiveCooldownLocked(name string) bool {
	expiry, ok := wp.agentCooldowns[name]
	if !ok {
		return false
	}
	now := time.Now()
	if now.After(expiry) {
		delete(wp.agentCooldowns, name)
		return false
	}
	clampedExpiry := wp.clampAgentCooldownExpiry(expiry, now)
	if clampedExpiry.Before(expiry) {
		wp.agentCooldowns[name] = clampedExpiry
	}
	return true
}

func (wp *WorkerPool) clampAgentCooldownExpiry(expiry, now time.Time) time.Time {
	maxExpiry := now.Add(wp.agentQuotaCooldown())
	if expiry.After(maxExpiry) {
		return maxExpiry
	}
	return expiry
}

func (wp *WorkerPool) failCooldownOrFailover(
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg string,
) {
	wp.failoverOrFailWithPrefix(workerID, job, agentName, errorMsg, review.QuotaErrorPrefix, "quota")
}

// failoverOrFail attempts failover to a backup agent for non-CI jobs. CI
// reviews must preserve their configured panel member and let the CI retry
// schedule handle quota/session availability failures.
func (wp *WorkerPool) failoverOrFail(
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg string,
) {
	wp.failoverOrFailWithPrefix(workerID, job, agentName, errorMsg, review.QuotaErrorPrefix, "quota")
}

func (wp *WorkerPool) failoverOrFailWithPrefix(
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg, prefix, label string,
) {
	backupAgent := ""
	if !job.IsCIReview() {
		backupAgent = wp.resolveBackupAgent(job)
	}
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
			log.Printf("[%s] Job %d failing over from %s to %s (%s): %s",
				workerID, job.ID, agentName, backupAgent, label, errorMsg)
			return
		}
	}

	wp.failJobWithPrefix(workerID, job, agentName, errorMsg, prefix, label)
}

func (wp *WorkerPool) failJobWithPrefix(
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg, prefix, label string,
) {
	storedMsg := prefixedFailure(prefix, errorMsg)
	if updated, err := wp.db.FailJob(job.ID, workerID, storedMsg); err != nil {
		log.Printf("[%s] Error failing job %d: %v", workerID, job.ID, err)
	} else if updated {
		log.Printf("[%s] Job %d skipped (agent %s %s)",
			workerID, job.ID, agentName, label)
		wp.broadcastFailed(job, agentName, storedMsg)
		if wp.errorLog != nil {
			wp.errorLog.LogError("worker",
				fmt.Sprintf("job %d skipped (%s): %s", job.ID, label, errorMsg),
				job.ID)
		}
		wp.logJobFailed(job.ID, workerID, agentName, storedMsg)
	}
}

func prefixedFailure(prefix, msg string) string {
	if prefix == "" || strings.HasPrefix(msg, prefix) {
		return msg
	}
	if prefix == review.OutageErrorPrefix {
		return review.OutageError(msg)
	}
	return prefix + msg
}

func preparePrebuiltPrompt(
	ctx context.Context, repoPath string, snapshotTarget prompt.SnapshotTarget,
	job *storage.ReviewJob, reviewPrompt string, excludes []string,
) (string, func(), error) {
	if !strings.Contains(reviewPrompt, prompt.DiffFilePathPlaceholder) {
		return reviewPrompt, nil, nil
	}
	builder := prompt.NewBuilder(nil).WithContext(ctx).ForRepo(repoPath, job.RepoID)
	diffFile, cleanup, err := builder.WriteDiffSnapshotTarget(job.GitRef, excludes, snapshotTarget)
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
