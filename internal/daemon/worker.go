package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

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

const (
	agentTimeoutErrorPrefix      = "agent timeout after"
	tokenUsageIndexRetryWindow   = 3 * time.Second
	tokenUsageIndexRetryInterval = 500 * time.Millisecond
)

type runningJobCancellation struct {
	cancel                context.CancelFunc
	callerBroadcastsEvent bool
}

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
	stopCtx       context.Context
	stopCancel    context.CancelFunc
	readyCh       chan struct{} // closed after wg.Add in Start
	startOnce     sync.Once
	stopOnce      sync.Once
	wg            sync.WaitGroup

	// Track running jobs for cancellation
	runningJobs    map[int64]runningJobCancellation
	pendingCancels map[int64]bool // job ID -> whether the caller broadcasts the event
	// updateInterruptTargets records attempts that must unwind without normal
	// cancellation, retry, failover, hook, or panel-completion side effects.
	// The daemon's update lease owns the lifetime of this set.
	updateInterruptTargets map[int64]struct{}
	failedUpdateRequeues   map[int64]string
	runningJobsMu          sync.Mutex
	// attemptTransitionsMu linearizes update-target registration with every
	// attempt-scoped retry, failover, failure, or completion transition.
	attemptTransitionsMu sync.RWMutex

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
	// tokenUsageIndexRetryWindow and tokenUsageIndexRetryInterval bound the
	// fresh-session indexing retry. Tests shorten both values.
	tokenUsageIndexRetryWindow   time.Duration
	tokenUsageIndexRetryInterval time.Duration
	tokenCostRetryCh             chan int64
	tokenCostScanInterval        time.Duration
	tokenCostRetryInterval       time.Duration
	tokenCostPageSize            int
	tokenCostImmediateAttempts   int
	tokenCostPendingLimit        int
	tokenCostMaxCandidateAge     time.Duration
	tokenUsageLogScanInterval    time.Duration
	tokenUsageLogPageSize        int

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
	stopCtx, stopCancel := context.WithCancel(context.Background())
	return &WorkerPool{
		db:                           db,
		cfgGetter:                    cfgGetter,
		broadcaster:                  broadcaster,
		errorLog:                     errorLog,
		activityLog:                  activityLog,
		numWorkers:                   numWorkers,
		stopCh:                       make(chan struct{}),
		stopCtx:                      stopCtx,
		stopCancel:                   stopCancel,
		readyCh:                      make(chan struct{}),
		runningJobs:                  make(map[int64]runningJobCancellation),
		pendingCancels:               make(map[int64]bool),
		updateInterruptTargets:       make(map[int64]struct{}),
		failedUpdateRequeues:         make(map[int64]string),
		agentCooldowns:               make(map[string]time.Time),
		outputBuffers:                NewOutputBuffer(512*1024, 4*1024*1024), // 512KB/job, 4MB total
		classify:                     agent.ClassifyLimit,
		retryBackoff:                 2 * time.Second,
		tokenUsageIndexRetryWindow:   tokenUsageIndexRetryWindow,
		tokenUsageIndexRetryInterval: tokenUsageIndexRetryInterval,
		tokenCostRetryCh:             make(chan int64, tokenCostRetryBufferSize),
		tokenCostScanInterval:        tokenCostScanInterval,
		tokenCostRetryInterval:       tokenCostRetryInterval,
		tokenCostPageSize:            tokenCostPageSize,
		tokenCostImmediateAttempts:   tokenCostImmediateAttempts,
		tokenCostPendingLimit:        tokenCostRetryBufferSize,
		tokenCostMaxCandidateAge:     tokenCostMaxCandidateAge,
		tokenUsageLogScanInterval:    tokenUsageLogScanInterval,
		tokenUsageLogPageSize:        tokenUsageLogPageSize,
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
		wp.wg.Add(wp.numWorkers + 1)
		close(wp.readyCh)
		go wp.runTokenCostReconciler()
		for i := 0; i < wp.numWorkers; i++ {
			go wp.worker(i)
		}
	})
}

// Stop gracefully shuts down the worker pool. Safe to call multiple times.
func (wp *WorkerPool) Stop() {
	wp.BeginStop()
	// Wait for Start to finish wg.Add before calling Wait.
	// If Start was never called, readyCh stays open and there is nothing to wait
	// for. Any later workers see the closed stopCh and exit immediately.
	select {
	case <-wp.readyCh:
		log.Println("Stopping worker pool...")
		wp.wg.Wait()
		log.Println("Worker pool stopped")
	default:
	}
}

// BeginStop synchronously prevents workers from claiming another job without
// waiting for currently active workers to finish.
func (wp *WorkerPool) BeginStop() {
	wp.stopOnce.Do(func() {
		wp.stopCancel()
		close(wp.stopCh)
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
	initial, ch, cancel := wp.outputBuffers.Subscribe(jobID)
	// Close a subscription that raced with attempt teardown. CloseJob removes
	// the live buffer, so a subscriber arriving just afterward can create a new
	// one; the authoritative status check turns that buffer into a closed stream.
	if job, err := wp.db.GetJobByID(jobID); err == nil && job.Status != storage.JobStatusRunning {
		wp.outputBuffers.CloseJob(jobID)
	}
	return initial, ch, cancel
}

// HasJobOutput returns true if there's active output capture for a job.
func (wp *WorkerPool) HasJobOutput(jobID int64) bool {
	return wp.outputBuffers.IsActive(jobID)
}

// CancelJob cancels a running job by its ID, killing the subprocess.
// Returns true if the job was canceled or marked for pending cancellation.
// Returns false only if the job doesn't exist or isn't in a cancellable state.
func (wp *WorkerPool) CancelJob(jobID int64) bool {
	return wp.cancelJob(jobID, false)
}

// cancelJob records whether another layer owns the terminal event. Direct
// worker-pool callers use CancelJob and leave the event to the worker.
func (wp *WorkerPool) cancelJob(jobID int64, callerBroadcastsEvent bool) bool {
	if cancel, ok := wp.registeredJobCancel(jobID, callerBroadcastsEvent); ok {
		log.Printf("Canceling job %d", jobID)
		cancel()
		return true
	}

	// Job not registered yet - check if it's a valid job before marking pending
	// This prevents unbounded growth of pendingCancels for invalid/finished job IDs
	// Note: we release the lock before the DB call to avoid blocking other operations
	job, err := wp.db.GetJobByID(jobID)
	if err != nil {
		// DB error - but job may have registered while we were trying to read
		// Re-check runningJobs before giving up
		if cancel, ok := wp.registeredJobCancel(jobID, callerBroadcastsEvent); ok {
			log.Printf("Canceling job %d (registered during failed DB check)", jobID)
			cancel()
			return true
		}
		return false
	}

	// Accept jobs that are queued, running, OR canceled-but-claimed (race condition case)
	// When db.CancelJob is called before workerPool.CancelJob, the status becomes 'canceled'
	// but the worker may not have registered yet. We detect this via WorkerID being set.
	if !wp.isJobCancellable(job) {
		return false
	}

	// Re-lock and check if job was registered while we were checking DB
	if cancel, ok := wp.registeredJobCancel(jobID, callerBroadcastsEvent); ok {
		log.Printf("Canceling job %d (registered during DB check)", jobID)
		cancel()
		return true
	}

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
	if running, ok := wp.runningJobs[jobID]; ok {
		running.callerBroadcastsEvent = running.callerBroadcastsEvent || callerBroadcastsEvent
		wp.runningJobs[jobID] = running
		wp.runningJobsMu.Unlock()
		log.Printf("Canceling job %d (registered during second DB check)", jobID)
		running.cancel()
		return true
	}

	// Mark for pending cancellation
	wp.pendingCancels[jobID] = wp.pendingCancels[jobID] || callerBroadcastsEvent
	wp.runningJobsMu.Unlock()
	log.Printf("Job %d not yet registered, marking for pending cancellation", jobID)
	return true
}

func (wp *WorkerPool) registeredJobCancel(
	jobID int64, callerBroadcastsEvent bool,
) (context.CancelFunc, bool) {
	wp.runningJobsMu.Lock()
	defer wp.runningJobsMu.Unlock()
	running, ok := wp.runningJobs[jobID]
	if !ok {
		return nil, false
	}
	running.callerBroadcastsEvent = running.callerBroadcastsEvent || callerBroadcastsEvent
	wp.runningJobs[jobID] = running
	return running.cancel, true
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
	callerBroadcastsEvent, pending := wp.pendingCancels[jobID]
	_, updateInterrupted := wp.updateInterruptTargets[jobID]
	wp.runningJobs[jobID] = runningJobCancellation{
		cancel: cancel, callerBroadcastsEvent: callerBroadcastsEvent,
	}

	// Check if this job was canceled before we registered it
	if pending || updateInterrupted {
		delete(wp.pendingCancels, jobID)
		wp.runningJobsMu.Unlock()
		if updateInterrupted {
			log.Printf("Job %d was targeted by an update, interrupting now", jobID)
		} else {
			log.Printf("Job %d was pending cancellation, canceling now", jobID)
		}
		cancel()
		return
	}
	wp.runningJobsMu.Unlock()
}

// InterruptJobsForUpdate marks the running attempts as update-owned and
// cancels any that have already registered their contexts. A job that
// registers after this call observes the target in registerRunningJob and is
// canceled there, closing the claim-to-registration race.
func (wp *WorkerPool) InterruptJobsForUpdate(jobIDs []int64) {
	wp.attemptTransitionsMu.Lock()
	defer wp.attemptTransitionsMu.Unlock()
	wp.interruptJobsForUpdateLocked(jobIDs)
}

// interruptJobsForUpdateLocked requires attemptTransitionsMu to be write
// locked. Update preparation holds that lock across the claim gate, running-job
// snapshot, and target registration so the three operations form one boundary.
func (wp *WorkerPool) interruptJobsForUpdateLocked(jobIDs []int64) {
	cancels := make([]context.CancelFunc, 0, len(jobIDs))
	wp.runningJobsMu.Lock()
	for _, jobID := range jobIDs {
		wp.updateInterruptTargets[jobID] = struct{}{}
		if running, ok := wp.runningJobs[jobID]; ok {
			cancels = append(cancels, running.cancel)
		}
	}
	wp.runningJobsMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// ClearUpdateInterruptTargets releases attempt markers after the daemon's
// update lease ends. It does not cancel or otherwise mutate jobs.
func (wp *WorkerPool) ClearUpdateInterruptTargets() {
	wp.runningJobsMu.Lock()
	clear(wp.updateInterruptTargets)
	clear(wp.failedUpdateRequeues)
	wp.runningJobsMu.Unlock()
}

// RetryFailedUpdateRequeues retries attempt-scoped transitions that failed
// while workers were unwinding. Callers must wait until the active worker
// count reaches zero so the old attempt cannot resume terminal handling after
// a successful retry.
func (wp *WorkerPool) RetryFailedUpdateRequeues() error {
	wp.runningJobsMu.Lock()
	pending := maps.Clone(wp.failedUpdateRequeues)
	wp.runningJobsMu.Unlock()

	var retryErr error
	for jobID, workerID := range pending {
		_, err := wp.db.RequeueUpdateInterruptedJob(jobID, workerID)
		if err != nil {
			retryErr = errors.Join(retryErr, fmt.Errorf("requeue job %d: %w", jobID, err))
			continue
		}
		wp.runningJobsMu.Lock()
		if wp.failedUpdateRequeues[jobID] == workerID {
			delete(wp.failedUpdateRequeues, jobID)
		}
		wp.runningJobsMu.Unlock()
	}
	return retryErr
}

// handleUpdateInterruption returns an update-owned attempt to the queue without
// consuming a retry or producing terminal side effects. The target marker is
// authoritative even before context cancellation becomes visible. The storage
// transition is guarded by both status and worker ID, so an ordinary user
// cancellation that wins the race is never overwritten.
func (wp *WorkerPool) handleUpdateInterruption(
	_ context.Context, workerID string, job *storage.ReviewJob,
) bool {
	wp.attemptTransitionsMu.RLock()
	defer wp.attemptTransitionsMu.RUnlock()
	return wp.handleUpdateInterruptionLocked(workerID, job)
}

// handleUpdateInterruptionLocked requires attemptTransitionsMu to be read
// locked so target registration cannot race the guarded storage transition.
func (wp *WorkerPool) handleUpdateInterruptionLocked(
	workerID string, job *storage.ReviewJob,
) bool {
	wp.runningJobsMu.Lock()
	_, targeted := wp.updateInterruptTargets[job.ID]
	wp.runningJobsMu.Unlock()
	if !targeted {
		return false
	}
	requeued, err := wp.db.RequeueUpdateInterruptedJob(job.ID, workerID)
	if err != nil {
		log.Printf("[%s] Error requeueing update-interrupted job %d: %v", workerID, job.ID, err)
		wp.runningJobsMu.Lock()
		wp.failedUpdateRequeues[job.ID] = workerID
		wp.runningJobsMu.Unlock()
		// Keep update-owned attempts out of normal failure and cancellation
		// handling even when the immediate transition fails. The row remains
		// running and replacement startup's stale-job recovery requeues it.
		return true
	}
	if requeued {
		log.Printf("[%s] Requeued update-interrupted job %d", workerID, job.ID)
	}
	return requeued
}

// runAttemptTransition holds the read side of the update cutover barrier for
// the marker check and the complete attempt-scoped transition. It returns true
// when update interruption handled the attempt instead of running transition.
func (wp *WorkerPool) runAttemptTransition(
	workerID string, job *storage.ReviewJob, transition func(),
) bool {
	wp.attemptTransitionsMu.RLock()
	defer wp.attemptTransitionsMu.RUnlock()
	if wp.handleUpdateInterruptionLocked(workerID, job) {
		return true
	}
	transition()
	return false
}

// IsJobPendingCancel reports whether a job is in the pendingCancels set.
// This is intended for use in tests.
func (wp *WorkerPool) IsJobPendingCancel(jobID int64) bool {
	wp.runningJobsMu.Lock()
	defer wp.runningJobsMu.Unlock()
	_, pending := wp.pendingCancels[jobID]
	return pending
}

func (wp *WorkerPool) cancellationEventOwnedByCaller(jobID int64) bool {
	wp.runningJobsMu.Lock()
	defer wp.runningJobsMu.Unlock()
	return wp.runningJobs[jobID].callerBroadcastsEvent
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

func (wp *WorkerPool) promptBuilderForJob(
	ctx context.Context,
	checkout preparedJobCheckout,
	job *storage.ReviewJob,
	cfg *config.Config,
) (*prompt.Builder, error) {
	builder := wp.basePromptBuilderForJob(ctx, checkout, job, cfg)
	if job.IsCIReview() && strings.TrimSpace(job.CIBaseBranch) != "" {
		repoConfig, err := loadCIRepoConfig(checkout.promptRepoPath)
		if err != nil {
			if !config.IsConfigParseError(err) {
				return nil, fmt.Errorf("load CI review config: %w", err)
			}
			log.Printf("worker: warning: failed to load CI review config: %v", err)
			repoConfig.Config = nil
		}
		builder = builder.WithRepoConfig(
			repoConfig.Config,
			repoConfig.Ref,
		)
	}
	return builder, nil
}

func (wp *WorkerPool) basePromptBuilderForJob(
	ctx context.Context,
	checkout preparedJobCheckout,
	job *storage.ReviewJob,
	cfg *config.Config,
) *prompt.Builder {
	builder := prompt.NewBuilderWithConfig(wp.db, cfg).
		WithContext(ctx).
		ForRepo(checkout.promptRepoPath, job.RepoID)
	if !job.IsCIReview() {
		builder = builder.WithKataClient(
			kata.NewCLIClient(checkout.promptRepoPath),
		)
	}
	return builder
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
	if job == nil || job.PanelRunUUID == nil || job.IsFixJob() {
		return false, nil
	}
	if job.Source == storage.JobSourceCI {
		return true, nil
	}
	if wp.db == nil {
		return false, nil
	}
	if _, err := wp.db.GetCIPanelByRunUUID(*job.PanelRunUUID); err != nil {
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
	wt, createErr := createWorkerWorktree(ctx, job.RepoPath, headRef, gitworktree.Options{
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
		job, err := wp.db.ClaimJobContext(wp.stopCtx, workerID)
		if err != nil {
			wp.noteClaimError(workerID, err)
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

// noteClaimError records a ClaimJob failure. SQLITE_BUSY / "database is
// locked" is lock contention: retry/backoff already happened inside
// ClaimJob, so do not spam the daemon error log. Empty-queue is silent
// (nil job, nil error). Real claim failures stay errors.
func (wp *WorkerPool) noteClaimError(workerID string, err error) {
	if storage.IsSQLiteBusy(err) {
		log.Printf("[%s] Claim job deferred (database busy): %v", workerID, err)
		return
	}
	log.Printf("[%s] Error claiming job: %v", workerID, err)
	if wp.errorLog != nil {
		wp.errorLog.LogError("worker", fmt.Sprintf("claim job: %v", err), 0)
	}
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
	defer wp.finishRunningJob(workerID, job.ID)
	// Every attempt owns the lifetime of its output stream, including paths that
	// fail before an agent starts and synthesis paths that do not invoke one.
	defer wp.outputBuffers.CloseJob(job.ID)

	jobUUID := uuid.Nil()
	if job.UUID != nil {
		jobUUID = *job.UUID
	}
	assignment, err := wp.db.GetExperimentAssignmentInputForJobUUID(jobUUID)
	if err != nil {
		wp.failOrRetryContext(ctx, workerID, job, job.Agent,
			fmt.Sprintf("load frozen experiment plan: %v", err))
		return
	}
	if assignment != nil {
		if err := applyFrozenExperimentSettings(job, assignment); err != nil {
			wp.failOrRetryContext(ctx, workerID, job, job.Agent,
				fmt.Sprintf("load frozen experiment plan: %v", err))
			return
		}
		job.FrozenExperimentPlan = assignment
	}

	// Synthesis jobs route to their own handler before the cooldown gate: the
	// all-failed and passthrough branches call no agent, so a synthesis-agent
	// quota cooldown must not skip or fail them. Placing this after
	// registerRunningJob keeps synthesis jobs cancellable.
	if job.IsSynthesisJob() {
		wp.processSynthesisJob(ctx, workerID, job)
		return
	}

	// Isolate persisted output at the start of the attempt, before checkout,
	// prompt, configuration, or cooldown failures can terminate it. A promoted
	// classifier row carries its classifier output into the design review once;
	// every other attempt starts with an empty log.
	appendJobLog := false
	if job.JobType == storage.JobTypeClassify {
		discardJobLogAppendMarker(job.ID)
	} else {
		appendJobLog = consumeJobLogAppendMarker(job.ID)
	}
	if !appendJobLog {
		if err := truncateJobLog(job.ID); err != nil {
			log.Printf("[%s] Warning: truncate job log for job %d: %v", workerID, job.ID, err)
		} else if err := RecordJobLogAgent(job.ID, job.Agent); err != nil {
			log.Printf("[%s] Warning: record agent for job log %d: %v", workerID, job.ID, err)
		}
	}

	// Skip immediately if the agent is in quota cooldown.
	// Resolve alias so "claude" checks cooldown for "claude-code".
	canonicalAgent := agent.CanonicalName(job.Agent)
	if wp.isAgentCoolingDown(canonicalAgent) {
		log.Printf("[%s] Agent %s in cooldown, skipping job %d",
			workerID, canonicalAgent, job.ID)
		wp.failCooldownOrFailoverContext(ctx, workerID, job, canonicalAgent,
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
		wp.failOrRetryContext(ctx, workerID, job, job.Agent, fmt.Sprintf("prepare checkout: %v", err))
		return
	}

	// Prompt-independent cleanup must not force a prebuilt CI job to reload
	// repository review configuration that it no longer needs.
	pb := wp.basePromptBuilderForJob(ctx, checkout, job, cfg)
	if err := pb.CleanupStaleSnapshots(prompt.DefaultStaleSnapshotAge); err != nil {
		log.Printf("[%s] Warning: cleanup stale snapshots for job %d: %v", workerID, job.ID, err)
	}
	var reviewPrompt string
	var promptToPersist string
	effectiveMinSeverity := job.MinSeverity
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
			wp.failOrRetryContext(ctx, workerID, job, job.Agent, fmt.Sprintf("prepare prebuilt prompt: %v", err))
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
		// CI rebuilds use the same default-branch config and template files as
		// enqueue-time prompt construction.
		pb, err = wp.promptBuilderForJob(ctx, checkout, job, cfg)
		if err != nil {
			log.Printf("[%s] Error configuring prompt builder: %v", workerID, err)
			wp.failOrRetryContext(ctx, workerID, job, job.Agent, fmt.Sprintf("configure prompt builder: %v", err))
			return
		}

		// Attributed jobs use the frozen plan verbatim, including an explicit
		// empty value. Ordinary jobs keep the legacy config cascade.
		if effectiveMinSeverity == "" && job.FrozenExperimentPlan == nil {
			resolved, resErr := config.ResolveReviewMinSeverity("", checkout.promptRepoPath, cfg)
			if resErr != nil {
				log.Printf("[%s] Error resolving min-severity: %v", workerID, resErr)
				wp.failOrRetryContext(ctx, workerID, job, job.Agent, fmt.Sprintf("resolve min-severity: %v", resErr))
				return
			}
			effectiveMinSeverity = resolved
		}

		if job.IsDirtyJob() {
			// Dirty job - use pre-captured diff
			diffContent := ""
			if job.DiffContent != nil {
				diffContent = *job.DiffContent
			}
			dirtyResult, dirtyErr := pb.BuildDirtyWithSnapshotTargetAndFiles(
				diffContent, job.DirtyFiles, cfg.ReviewContextCount, job.Agent, job.ReviewType, effectiveMinSeverity,
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
				job.ReviewType, effectiveMinSeverity, excludes, checkout.snapshotTarget,
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
		wp.failOrRetryContext(ctx, workerID, job, job.Agent, fmt.Sprintf("build prompt: %v", err))
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
	baseAgent, err := resolveReviewJobAgent(job, cfg)
	if err != nil {
		log.Printf("[%s] Error getting agent: %v", workerID, err)
		wp.failOrRetryAgentContext(ctx, workerID, job, job.Agent, fmt.Sprintf("get agent: %v", err))
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
		if sa, ok := a.(agent.SessionAgent); ok {
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
		wp.failoverOrFailNonRetryableAgentContext(
			ctx, workerID, job, agentName,
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
	if appendJobLog {
		jobLog = newAppendingJobLogWriter(job.ID)
	} else {
		jobLog = newAgentJobLogWriter(job.ID, agentName)
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
		wt, wtErr := createWorkerWorktree(ctx, job.RepoPath, job.GitRef, gitworktree.Options{
			Prefix:         "roborev-worktree-",
			InitSubmodules: true,
			PullLFS:        true,
		})
		if wtErr != nil {
			log.Printf("[%s] Error creating worktree for fix job %d: %v", workerID, job.ID, wtErr)
			wp.failOrRetryContext(ctx, workerID, job, agentName, fmt.Sprintf("create worktree: %v", wtErr))
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
	agentReview, err := review.RunAgentReview(
		ctx, a, reviewRepoPath, job.GitRef, reviewPrompt, job.ReviewType,
		effectiveMinSeverity, agentOutput,
	)
	output := agentReview.Output
	sessionWriter.Flush()
	if sessionID := sessionWriter.SessionID(); sessionID != "" {
		if saveErr := wp.db.SaveJobSessionID(job.ID, workerID, sessionID); saveErr != nil {
			log.Printf("[%s] Error persisting session ID for job %d: %v", workerID, job.ID, saveErr)
		}
	}
	if err != nil {
		// Check if this was a cancellation
		if ctx.Err() == context.Canceled {
			if wp.handleUpdateInterruption(ctx, workerID, job) {
				return
			}
			log.Printf("[%s] Job %d was canceled", workerID, job.ID)
			if !wp.cancellationEventOwnedByCaller(job.ID) {
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
			}
			// Member canceled is terminal — release the panel synthesis.
			wp.releaseIfPanelMember(job)
			return // Job already marked as canceled in DB, nothing more to do
		}
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			timeoutErr := fmt.Sprintf(
				"%s %s",
				agentTimeoutErrorPrefix,
				timeoutDuration.Round(time.Second),
			)
			log.Printf("[%s] Job %d timed out: %v", workerID, job.ID, err)
			wp.failOrRetryAgentContext(ctx, workerID, job, agentName, timeoutErr)
			return
		}
		log.Printf("[%s] Agent error on job %d: %v",
			workerID, job.ID, err)
		wp.failOrRetryAgentExecutionContext(ctx, workerID, job, agentName, err)
		return
	}
	if wp.handleUpdateInterruption(ctx, workerID, job) {
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
			wp.failOrRetryContext(ctx, workerID, job, agentName, fmt.Sprintf("patch capture: %v", patchErr))
			return
		}
		if fixPatch == "" {
			log.Printf("[%s] Fix job %d: agent produced no file changes", workerID, job.ID)
			wp.failOrRetryContext(ctx, workerID, job, agentName, "agent produced no file changes")
			return
		}
		log.Printf("[%s] Fix job %d: captured patch (%d bytes)", workerID, job.ID, len(fixPatch))
	}

	// For compact jobs, validate raw agent output before storing.
	// Invalid output (empty, error patterns) should fail the job,
	// not produce a "done" review that misleads --wait callers.
	if job.JobType == "compact" && !IsValidCompactOutput(output) {
		log.Printf("[%s] Compact job %d produced invalid output, failing", workerID, job.ID)
		wp.failOrRetryAgentContext(ctx, workerID, job, agentName, "compact output invalid (empty or error)")
		return
	}

	wp.runAttemptTransition(workerID, job, func() {
		// Store the result (use actual agent name, not requested).
		// CompleteJob/CompleteFixJob is a no-op (returns nil) if the job was
		// canceled between agent finish and now.
		if job.IsFixJob() {
			if err := wp.db.CompleteFixJob(job.ID, agentName, reviewPrompt, output, fixPatch); err != nil {
				log.Printf("[%s] Error storing fix review: %v", workerID, err)
				return
			}
		} else if job.IsReviewJob() &&
			agentReview.Verdict != storage.VerdictUnknown {
			if err := wp.db.CompleteJobResult(
				job.ID, agentName, reviewPrompt, storage.ReviewCompletion{
					Output:           output,
					Verdict:          agentReview.Verdict,
					StructuredOutput: agentReview.StructuredOutput,
					MinSeverity:      effectiveMinSeverity,
				},
			); err != nil {
				log.Printf("[%s] Error storing review verdict: %v", workerID, err)
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

		verdict := agentReview.Verdict
		if verdict == storage.VerdictUnknown {
			verdict = storage.ParseVerdict(output)
		}
		wp.autoClosePassingReview(workerID, job, verdict)

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
			Verdict:      string(verdict),
			Findings:     output,
			WorktreePath: eventWorktreePath,
		})
	})
}

func (wp *WorkerPool) finishRunningJob(workerID string, jobID int64) {
	// Remove the old cancellation handler before releasing the database row.
	// Once worker_id becomes NULL, a rerun may be claimed and register a new
	// handler for the same job ID.
	wp.unregisterRunningJob(jobID)
	if _, err := wp.db.ReleaseCanceledJob(jobID, workerID); err != nil {
		log.Printf("[%s] Error releasing canceled job %d: %v", workerID, jobID, err)
	}
}

func (wp *WorkerPool) autoClosePassingReview(
	workerID string, job *storage.ReviewJob, verdict storage.Verdict,
) {
	if !job.IsReviewJob() && !job.IsSynthesisJob() {
		return
	}
	if verdict != "P" {
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
	if !job.IsReviewJob() && !job.UsesStoredPrompt() {
		return a
	}
	a = agent.WithCodexSkillsDisabled(
		a,
		config.ResolveDisableCodexReviewSkills(job.RepoPath, cfg),
	)
	if !job.IsReviewJob() {
		return a
	}
	a = agent.WithCodexUserConfigIgnored(
		a,
		config.ResolveIgnoreCodexReviewUserConfig(job.RepoPath, cfg),
	)
	return a
}

// failOrRetry attempts to retry the job, or marks it as failed if max retries reached.
// This is used for non-agent errors (e.g., prompt build failures) where switching agents won't help.
func (wp *WorkerPool) failOrRetry(workerID string, job *storage.ReviewJob, agentName string, errorMsg string) {
	wp.failOrRetryContext(context.Background(), workerID, job, agentName, errorMsg)
}

func (wp *WorkerPool) failOrRetryContext(
	_ context.Context, workerID string, job *storage.ReviewJob, agentName string, errorMsg string,
) {
	wp.runAttemptTransition(workerID, job, func() {
		wp.failOrRetryInnerLocked(workerID, job, agentName, errorMsg, false, nil)
	})
}

// failOrRetryAgent is like failOrRetry but allows failover to a backup agent
// when retries are exhausted. Used for agent-execution errors where switching
// agents may resolve the issue.
func (wp *WorkerPool) failOrRetryAgent(workerID string, job *storage.ReviewJob, agentName string, errorMsg string) {
	wp.failOrRetryAgentContext(context.Background(), workerID, job, agentName, errorMsg)
}

func (wp *WorkerPool) failOrRetryAgentContext(
	_ context.Context, workerID string, job *storage.ReviewJob, agentName string, errorMsg string,
) {
	wp.runAttemptTransition(workerID, job, func() {
		wp.failOrRetryInnerLocked(workerID, job, agentName, errorMsg, true, nil)
	})
}

// failOrRetryAgentExecutionContext preserves the typed category attached by an
// agent adapter until existing provider-limit classification has run. Unknown
// pre-protocol failures skip same-agent retries and use the non-retryable
// backup path; recognized quota, session, and transient signals retain their
// existing behavior.
func (wp *WorkerPool) failOrRetryAgentExecutionContext(
	ctx context.Context,
	workerID string,
	job *storage.ReviewJob,
	agentName string,
	executionErr error,
) {
	errorMsg := fmt.Sprintf("agent: %v", executionErr)
	classification, attached := agent.LimitClassificationFromError(executionErr)
	if !attached {
		classification = wp.classify(agent.CanonicalName(agentName), errorMsg)
	}
	if classification.Kind == agent.LimitKindNone && agent.IsUnavailable(executionErr) {
		wp.failoverOrFailNonRetryableAgentContext(
			ctx, workerID, job, agentName, review.UnavailableError(errorMsg),
		)
		return
	}
	if attached && classification.Kind == agent.LimitKindTransient {
		errorMsg = review.OutageError(errorMsg)
	}
	wp.runAttemptTransition(workerID, job, func() {
		wp.failOrRetryInnerLocked(
			workerID, job, agentName, errorMsg, true, &classification,
		)
	})
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
	wp.runAttemptTransition(workerID, job, func() {
		wp.failOrRetryInnerLocked(workerID, job, agentName, errorMsg, agentError, nil)
	})
}

func (wp *WorkerPool) failOrRetryInnerLocked(
	workerID string,
	job *storage.ReviewJob,
	agentName string,
	errorMsg string,
	agentError bool,
	classificationOverride *agent.LimitClassification,
) {
	// Quota and session-limit errors skip retries entirely — cool down
	// the agent and attempt failover or fail. Behavior matches the
	// original isQuotaError branch; classification now lives in
	// internal/agent (ClassifyLimit) so the CLI fix loop can share it.
	if agentError {
		cls := wp.classify(agent.CanonicalName(agentName), errorMsg)
		if classificationOverride != nil {
			cls = *classificationOverride
		}
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
			wp.failoverOrFailWithPrefixLocked(workerID, job, agentName, errorMsg, prefix, label)
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
		wp.failoverOrFailNonRetryableAgentLocked(workerID, job, agentName, errorMsg)
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

func (wp *WorkerPool) failoverOrFailNonRetryableAgentContext(
	_ context.Context,
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg string,
) {
	wp.runAttemptTransition(workerID, job, func() {
		wp.failoverOrFailNonRetryableAgentLocked(workerID, job, agentName, errorMsg)
	})
}

func (wp *WorkerPool) failoverOrFailNonRetryableAgentLocked(
	workerID string, job *storage.ReviewJob, agentName, errorMsg string,
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
// agent/model resolution. Fix jobs map to "fix"; specialized review
// jobs use their review-type workflow mapping.
func failoverWorkflow(job *storage.ReviewJob) string {
	if job.IsFixJob() {
		return "fix"
	}
	return config.WorkflowForReviewType(job.ReviewType)
}

// resolveBackupAgent determines the backup agent for a job. A frozen
// experiment plan is authoritative, including an explicit empty backup.
// Ordinary jobs retain the workflow-config fallback.
func (wp *WorkerPool) resolveBackupAgent(job *storage.ReviewJob) string {
	// An explicit per-job backup wins, for reliability: the enqueuer (e.g. a CI
	// panel synthesis) chose this failover deliberately. Canonicalize the alias
	// (e.g. "claude" -> "claude-code") via the registry; fall back to the raw
	// value if the name is unknown so the override is never silently dropped.
	// Note: agent.Get resolves from the registry, not PATH, so this does not
	// require local installation.
	if job.FrozenExperimentPlan != nil && job.BackupAgent == "" {
		return ""
	}
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
	// Resolve exactly the configured backup using the config-aware path so
	// command overrides and configured ACP aliases participate in failover
	// without falling through to unrelated agents.
	resolved, err := agent.GetAvailableExactWithConfig(job.RepoPath, backup, cfg)
	if err != nil {
		return ""
	}
	if resolution.AgentMatches(resolved.Name(), job.Agent) {
		return ""
	}
	return resolved.Name()
}

// resolveBackupModel returns the frozen experiment value when present.
// Ordinary jobs retain the workflow-config fallback.
func (wp *WorkerPool) resolveBackupModel(job *storage.ReviewJob) string {
	// Stored backup agent present => use the stored model, even if empty. Never
	// fall through to the workflow backup model, which is resolved for a
	// different agent (the user's F7 guardrail).
	if job.FrozenExperimentPlan != nil {
		return job.BackupModel
	}
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
	backup := strings.TrimSpace(resolution.BackupAgent)
	if backup == "" {
		return ""
	}
	// Resolve the model for the concrete backup agent through the same
	// chokepoint the enqueue paths use so the ACP backup pairing guard
	// applies: an inherited global default_backup_model must not be
	// persisted for a mispaired ACP backup agent, or the failover attempt
	// would hand ACP a model it never advertised and fail again.
	return resolution.ModelForSelectedAgent(backup, "")
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
	if job.PanelRole == storage.PanelRoleMember && job.PanelRunUUID != nil {
		if err := wp.db.MaybeReleasePanelSynthesis(*job.PanelRunUUID); err != nil {
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

	var logUsage *tokens.Usage
	var providerUsage *tokens.Usage
	logUsage, logErr := tokens.ParseCodexUsageFile(JobLogPath(job.ID))
	if logErr != nil {
		log.Printf("[%s] Warning: parse token usage from job log for job %d: %v",
			workerID, job.ID, logErr)
	}

	if capturedSession != "" && !wasResumed {
		fetched, tokenErr := wp.fetchFreshSessionUsage(
			ctx, wp.fetchTokenUsage, capturedSession,
		)
		switch {
		case tokenErr == nil:
			providerUsage = fetched
		case !errors.Is(tokenErr, tokens.ErrUsageProviderUnavailable):
			log.Printf("[%s] Warning: fetch token usage for job %d: %v",
				workerID, job.ID, tokenErr)
		}
	}
	usage := backfill.MergeTokenUsage(tokens.ToJSON(logUsage), providerUsage)
	needsLateCost := capturedSession != "" && !wasResumed &&
		backfill.NeedsTokenCostBackfill(tokens.ToJSON(usage))
	if needsLateCost {
		wp.queueTokenCostRetry(job.ID)
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
	current, err := wp.db.GetJobByID(job.ID)
	if err != nil {
		log.Printf("[%s] Warning: reload job %d before saving token usage: %v",
			workerID, job.ID, err)
		return
	}
	_, _, err = backfill.StoreCapturedTokenUsage(
		wp.db,
		backfill.CapturedUsage{
			JobID:             job.ID,
			SessionID:         sessionID,
			ExistingJSON:      current.TokenUsage,
			ExpectedStartedAt: job.StartedAtRaw,
		},
		logUsage,
		providerUsage,
	)
	if err != nil {
		log.Printf("[%s] Warning: save token usage for job %d: %v",
			workerID, job.ID, err)
		if capturedSession != "" && !wasResumed {
			wp.queueTokenCostRetry(job.ID)
		}
	}
}

func (wp *WorkerPool) fetchTokenUsage(
	ctx context.Context, sessionID string,
) (*tokens.Usage, error) {
	if wp.tokenUsageFetcher != nil {
		return wp.tokenUsageFetcher(ctx, sessionID)
	}
	cfg := wp.cfgGetter.Config()
	return tokens.FetchForSessionWithConfig(
		ctx, sessionID,
		tokens.FetchConfig{
			Endpoint:   cfg.Cost.Endpoint,
			Timeout:    cfg.Cost.ResolvedTimeout(),
			RequireCLI: true,
		},
	)
}

func (wp *WorkerPool) fetchFreshSessionUsage(
	ctx context.Context,
	fetcher func(context.Context, string) (*tokens.Usage, error),
	sessionID string,
) (*tokens.Usage, error) {
	retryCtx, cancel := context.WithTimeout(ctx, wp.tokenUsageIndexRetryWindow)
	defer cancel()

	for {
		usage, err := fetcher(retryCtx, sessionID)
		if err != nil {
			if retryCtx.Err() != nil {
				return nil, nil
			}
			return nil, err
		}
		if usage != nil {
			return usage, nil
		}

		timer := time.NewTimer(wp.tokenUsageIndexRetryInterval)
		select {
		case <-retryCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, nil
		case <-timer.C:
		}
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

func (wp *WorkerPool) failCooldownOrFailoverContext(
	_ context.Context,
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg string,
) {
	wp.failoverOrFailWithPrefixContext(workerID, job, agentName, errorMsg, review.QuotaErrorPrefix, "quota")
}

// failoverOrFail attempts failover to a backup agent for non-CI jobs. CI
// reviews must preserve their configured panel member and let the CI retry
// schedule handle quota/session availability failures.
func (wp *WorkerPool) failoverOrFail(
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg string,
) {
	wp.failoverOrFailWithPrefixContext(workerID, job, agentName, errorMsg, review.QuotaErrorPrefix, "quota")
}

func (wp *WorkerPool) failoverOrFailWithPrefixContext(
	workerID string, job *storage.ReviewJob,
	agentName, errorMsg, prefix, label string,
) {
	wp.runAttemptTransition(workerID, job, func() {
		wp.failoverOrFailWithPrefixLocked(workerID, job, agentName, errorMsg, prefix, label)
	})
}

func (wp *WorkerPool) failoverOrFailWithPrefixLocked(
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

	wp.failJobWithPrefixLocked(workerID, job, agentName, errorMsg, prefix, label)
}

func (wp *WorkerPool) failJobWithPrefixLocked(
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
