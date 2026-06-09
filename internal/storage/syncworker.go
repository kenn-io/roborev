package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/config"
)

// SyncWorker handles background synchronization between SQLite and PostgreSQL
type SyncWorker struct {
	db              *DB
	cfg             config.SyncConfig
	pgPool          *PgPool
	stopCh          chan struct{}
	doneCh          chan struct{}
	mu              sync.Mutex // protects running and pgPool
	syncMu          sync.Mutex // serializes sync operations (doSync, SyncNow, FinalPush)
	connectMu       sync.Mutex // serializes connect operations
	running         bool
	skipInitialSync bool // when true, skip the immediate doSync on connect
}

// NewSyncWorker creates a new sync worker
func NewSyncWorker(db *DB, cfg config.SyncConfig) *SyncWorker {
	return &SyncWorker{
		db:     db,
		cfg:    cfg,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// SetSkipInitialSync disables the immediate doSync that normally
// runs right after connecting. Must be called before Start().
func (w *SyncWorker) SetSkipInitialSync(skip bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		return fmt.Errorf("cannot set skipInitialSync after Start()")
	}
	w.skipInitialSync = skip
	return nil
}

// Start begins the sync worker in a background goroutine.
// The worker can be stopped with Stop() and restarted with Start().
func (w *SyncWorker) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return fmt.Errorf("sync worker already running")
	}

	if !w.cfg.Enabled {
		return fmt.Errorf("sync is not enabled in config")
	}

	// Parse sync interval
	interval, err := time.ParseDuration(w.cfg.Interval)
	if err != nil || interval <= 0 {
		interval = time.Hour // Default
	}

	// Parse connect timeout
	connectTimeout, err := time.ParseDuration(w.cfg.ConnectTimeout)
	if err != nil || connectTimeout <= 0 {
		connectTimeout = 5 * time.Second // Default
	}

	// Reinitialize channels for Start→Stop→Start cycles
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	w.stopCh = stopCh
	w.doneCh = doneCh

	w.running = true
	// Snapshot skipInitialSync under lock so the goroutine reads
	// an immutable copy — no race with a hypothetical late setter.
	skipSync := w.skipInitialSync
	// Pass channels as parameters to avoid data races if Start() is called
	// again while the goroutine is still running (which shouldn't happen,
	// but this makes it safe regardless)
	go w.run(stopCh, doneCh, interval, connectTimeout, skipSync)
	return nil
}

// Stop gracefully stops the sync worker
func (w *SyncWorker) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	// Capture channels and set running=false while holding lock.
	// This prevents races with concurrent Start() which could reinitialize
	// the channels after we unlock but before we close them.
	stopCh := w.stopCh
	doneCh := w.doneCh
	w.running = false
	w.mu.Unlock()

	close(stopCh)
	<-doneCh

	// Acquire syncMu to wait for any in-flight SyncNow or FinalPush to complete
	// before closing the pool
	w.syncMu.Lock()
	defer w.syncMu.Unlock()

	w.mu.Lock()
	if w.pgPool != nil {
		w.pgPool.Close()
		w.pgPool = nil
	}
	w.mu.Unlock()
}

// SyncStats contains statistics from a sync operation
type SyncStats struct {
	PushedJobs      int
	PushedReviews   int
	PushedResponses int
	PulledJobs      int
	PulledReviews   int
	PulledResponses int
}

// FinalPush performs a push-only sync for graceful shutdown.
// This ensures all local changes are pushed before the daemon exits.
// Loops until all pending items are synced (not just one batch).
// Does not pull changes since we're shutting down.
func (w *SyncWorker) FinalPush() error {
	w.mu.Lock()
	pool := w.pgPool
	w.mu.Unlock()

	if pool == nil {
		return nil // Not connected, nothing to push
	}

	// Serialize with other sync operations
	w.syncMu.Lock()
	defer w.syncMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Loop until all pending items are pushed
	var totalJobs, totalReviews, totalResponses int
	for {
		stats, err := w.pushChangesWithStats(ctx, pool)
		if err != nil {
			return fmt.Errorf("final push: %w", err)
		}

		totalJobs += stats.Jobs
		totalReviews += stats.Reviews
		totalResponses += stats.Responses

		// If no items were pushed this round, we're done
		if stats.Jobs == 0 && stats.Reviews == 0 && stats.Responses == 0 {
			break
		}
	}

	if totalJobs > 0 || totalReviews > 0 || totalResponses > 0 {
		log.Printf("Sync: final push completed (%d jobs, %d reviews, %d responses)",
			totalJobs, totalReviews, totalResponses)
	}

	return nil
}

// SyncProgress reports progress during a sync operation
type SyncProgress struct {
	Phase      string // "push" or "pull"
	BatchNum   int
	BatchJobs  int
	BatchRevs  int
	BatchResps int
	TotalJobs  int
	TotalRevs  int
	TotalResps int
}

// SyncNow triggers an immediate sync cycle and returns statistics.
// Returns an error if the worker is not running or not connected.
// Loops until all pending items are pushed (not just one batch).
func (w *SyncWorker) SyncNow() (*SyncStats, error) {
	return w.SyncNowWithProgress(nil)
}

// SyncNowWithProgress is like SyncNow but calls progressFn after each batch.
// If not connected, attempts to connect first.
func (w *SyncWorker) SyncNowWithProgress(progressFn func(SyncProgress) bool) (*SyncStats, error) {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return nil, fmt.Errorf("sync worker not running")
	}
	pool := w.pgPool
	w.mu.Unlock()

	// If not connected, attempt to connect now
	if pool == nil {
		connectTimeout := 30 * time.Second
		if _, err := w.connect(connectTimeout); err != nil {
			// Log full error server-side, return generic error to caller
			// (connection errors may contain credentials or host details)
			log.Printf("Sync: connection failed: %v", err)
			return nil, fmt.Errorf("failed to connect to PostgreSQL")
		}
		// Re-check running state and get pool under lock
		// (Stop may have been called during connect)
		w.mu.Lock()
		if !w.running {
			// Worker was stopped during connect - close pool and abort
			if w.pgPool != nil {
				w.pgPool.Close()
				w.pgPool = nil
			}
			w.mu.Unlock()
			return nil, fmt.Errorf("sync worker stopped during connect")
		}
		pool = w.pgPool
		w.mu.Unlock()
		if pool == nil {
			return nil, fmt.Errorf("connection succeeded but pool is nil")
		}
		log.Printf("Sync: connected to PostgreSQL (triggered by sync now)")
	}

	// Serialize with other sync operations
	w.syncMu.Lock()
	defer w.syncMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stats := &SyncStats{}

	// Push local changes (limited batches to ensure pull still runs)
	// Cap at 20 batches (500 items at 25/batch) to prevent infinite push loops
	// if new jobs keep arriving during sync.
	const maxPushBatches = 20
	batchNum := 0
	for batchNum < maxPushBatches {
		pushed, err := w.pushChangesWithStats(ctx, pool)
		if err != nil {
			return nil, fmt.Errorf("push: %w", err)
		}
		stats.PushedJobs += pushed.Jobs
		stats.PushedReviews += pushed.Reviews
		stats.PushedResponses += pushed.Responses

		// If nothing was pushed, we're done with push phase
		if pushed.Jobs == 0 && pushed.Reviews == 0 && pushed.Responses == 0 {
			break
		}

		batchNum++
		log.Printf("Sync: batch %d - pushed %d jobs, %d reviews, %d responses (total: %d/%d/%d)",
			batchNum, pushed.Jobs, pushed.Reviews, pushed.Responses,
			stats.PushedJobs, stats.PushedReviews, stats.PushedResponses)

		if progressFn != nil {
			if !progressFn(SyncProgress{
				Phase:      "push",
				BatchNum:   batchNum,
				BatchJobs:  pushed.Jobs,
				BatchRevs:  pushed.Reviews,
				BatchResps: pushed.Responses,
				TotalJobs:  stats.PushedJobs,
				TotalRevs:  stats.PushedReviews,
				TotalResps: stats.PushedResponses,
			}) {
				return stats, nil
			}
		}
	}
	if batchNum == maxPushBatches {
		log.Printf("Sync: reached max push batches (%d), proceeding to pull", maxPushBatches)
	}

	// Pull remote changes
	pulled, err := w.pullChangesWithStats(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}
	stats.PulledJobs = pulled.Jobs
	stats.PulledReviews = pulled.Reviews
	stats.PulledResponses = pulled.Responses

	if progressFn != nil && (pulled.Jobs > 0 || pulled.Reviews > 0 || pulled.Responses > 0) {
		if !progressFn(SyncProgress{
			Phase:      "pull",
			TotalJobs:  pulled.Jobs,
			TotalRevs:  pulled.Reviews,
			TotalResps: pulled.Responses,
		}) {
			return stats, nil
		}
	}

	if stats.PushedJobs > 0 || stats.PushedReviews > 0 || stats.PushedResponses > 0 ||
		stats.PulledJobs > 0 || stats.PulledReviews > 0 || stats.PulledResponses > 0 {
		log.Printf("Sync: complete - pushed %d/%d/%d, pulled %d/%d/%d",
			stats.PushedJobs, stats.PushedReviews, stats.PushedResponses,
			stats.PulledJobs, stats.PulledReviews, stats.PulledResponses)
	}

	return stats, nil
}

// pushPullStats tracks counts for push/pull operations
type pushPullStats struct {
	Jobs      int
	Reviews   int
	Responses int
}

// run is the main sync loop
func (w *SyncWorker) run(stopCh, doneCh chan struct{}, interval, connectTimeout time.Duration, skipInitialSync bool) {
	defer close(doneCh)

	// Initial connection attempt with backoff
	backoff := time.Second
	maxBackoff := 5 * time.Minute

	for {
		select {
		case <-stopCh:
			return
		default:
		}

		// Try to connect
		newConn, err := w.connect(connectTimeout)
		if err != nil {
			log.Printf("Sync: connection failed: %v (retry in %v)", err, backoff)
			select {
			case <-stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Connected - reset backoff
		backoff = time.Second
		if newConn {
			log.Printf("Sync: connected to PostgreSQL")
		}

		// Run sync loop until disconnection or stop
		// Only do initial sync if we made the connection (not if SyncNow connected for us)
		// and skipInitialSync is not set
		w.syncLoop(stopCh, interval, newConn && !skipInitialSync)

		// If we get here, we disconnected - try to reconnect
		w.mu.Lock()
		if w.pgPool != nil {
			w.pgPool.Close()
			w.pgPool = nil
		}
		w.mu.Unlock()
	}
}

// connect establishes the PostgreSQL connection.
// Serialized by connectMu to prevent concurrent connection attempts.
// Returns (true, nil) if a new connection was made, (false, nil) if already connected.
func (w *SyncWorker) connect(timeout time.Duration) (bool, error) {
	w.connectMu.Lock()
	defer w.connectMu.Unlock()

	// Check if already connected (another goroutine may have connected while we waited)
	w.mu.Lock()
	if w.pgPool != nil {
		w.mu.Unlock()
		return false, nil
	}
	w.mu.Unlock()

	url := w.cfg.PostgresURLExpanded()
	if url == "" {
		return false, fmt.Errorf("postgres_url not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cfg := DefaultPgPoolConfig()
	cfg.ConnectTimeout = timeout

	pool, err := NewPgPool(ctx, url, cfg)
	if err != nil {
		return false, err
	}

	// Ensure schema exists
	if err := pool.EnsureSchema(ctx); err != nil {
		pool.Close()
		return false, fmt.Errorf("ensure schema: %w", err)
	}

	// Check if this is a new/different Postgres database
	dbID, err := pool.GetDatabaseID(ctx)
	if err != nil {
		pool.Close()
		return false, fmt.Errorf("get database ID: %w", err)
	}

	lastTargetID, err := w.db.GetSyncState(SyncStateSyncTargetID)
	if err != nil {
		pool.Close()
		return false, fmt.Errorf("get sync target ID: %w", err)
	}

	if lastTargetID != "" && lastTargetID != dbID {
		// Different database - clear all synced_at and pull cursors for full re-sync
		oldID, newID := lastTargetID, dbID
		if len(oldID) > 8 {
			oldID = oldID[:8]
		}
		if len(newID) > 8 {
			newID = newID[:8]
		}
		log.Printf("Sync: detected new Postgres database (was %s, now %s), clearing sync state for full re-sync", oldID, newID)
		if err := w.db.ClearAllSyncedAt(); err != nil {
			pool.Close()
			return false, fmt.Errorf("clear synced_at: %w", err)
		}
		// Also clear pull cursors so we pull all data from the new database
		for _, key := range []string{SyncStateLastJobCursor, SyncStateLastReviewCursor, SyncStateLastResponseID} {
			if err := w.db.SetSyncState(key, ""); err != nil {
				pool.Close()
				return false, fmt.Errorf("clear %s: %w", key, err)
			}
		}
	}

	// Update the sync target ID
	if err := w.db.SetSyncState(SyncStateSyncTargetID, dbID); err != nil {
		pool.Close()
		return false, fmt.Errorf("set sync target ID: %w", err)
	}

	// Register this machine
	machineID, err := w.db.GetMachineID()
	if err != nil {
		pool.Close()
		return false, fmt.Errorf("get machine ID: %w", err)
	}

	if err := pool.RegisterMachine(ctx, machineID, w.cfg.MachineName); err != nil {
		pool.Close()
		return false, fmt.Errorf("register machine: %w", err)
	}

	w.mu.Lock()
	w.pgPool = pool
	w.mu.Unlock()

	return true, nil
}

// syncLoop runs the periodic sync until stop or disconnection
// doInitialSync controls whether to sync immediately on entry (only when we made the connection)
func (w *SyncWorker) syncLoop(stopCh <-chan struct{}, interval time.Duration, doInitialSync bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Do initial sync immediately only if we made the connection
	// (if SyncNow connected, it will handle its own sync)
	if doInitialSync {
		if err := w.doSync(); err != nil {
			log.Printf("Sync: error: %v", err)
		}
	}

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if err := w.doSync(); err != nil {
				log.Printf("Sync: error: %v", err)
				// Check if connection is still alive - grab pool under lock
				w.mu.Lock()
				pool := w.pgPool
				w.mu.Unlock()
				if pool != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					if pool.Ping(ctx) != nil {
						cancel()
						return // Connection lost, exit to reconnect
					}
					cancel()
				}
			}
		}
	}
}

// doSync performs a single sync cycle (push then pull)
// Loops until all pending items are pushed.
func (w *SyncWorker) doSync() error {
	w.syncMu.Lock()
	defer w.syncMu.Unlock()

	w.mu.Lock()
	pool := w.pgPool
	w.mu.Unlock()

	if pool == nil {
		return fmt.Errorf("not connected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Push all local changes to PostgreSQL (loop until no more pending)
	var totalJobs, totalReviews, totalResponses int
	for {
		stats, err := w.pushChangesWithStats(ctx, pool)
		if err != nil {
			return fmt.Errorf("push: %w", err)
		}
		if stats.Jobs == 0 && stats.Reviews == 0 && stats.Responses == 0 {
			break
		}
		totalJobs += stats.Jobs
		totalReviews += stats.Reviews
		totalResponses += stats.Responses
		log.Printf("Sync: pushed %d jobs, %d reviews, %d responses (total: %d/%d/%d)",
			stats.Jobs, stats.Reviews, stats.Responses, totalJobs, totalReviews, totalResponses)
	}

	// Pull remote changes from PostgreSQL
	if err := w.pullChanges(ctx, pool); err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	return nil
}

// syncBatchSize controls how many items are pushed per batch.
// Smaller batches mean more frequent commits and better progress visibility.
const syncBatchSize = 25

// defaultSyncCursorLookback re-reads a timestamp overlap to avoid missing rows
// whose transaction becomes visible after a newer timestamp has advanced the
// local cursor. PostgreSQL NOW() is transaction-start time, so visible rows can
// arrive behind the cursor when a transaction commits late.
const defaultSyncCursorLookback = 5 * time.Minute

const syncCursorLookbackEnv = "ROBOREV_SYNC_CURSOR_LOOKBACK"

// pushChangesWithStats pushes local changes and returns statistics
func (w *SyncWorker) pushChangesWithStats(ctx context.Context, pool *PgPool) (pushPullStats, error) {
	stats := pushPullStats{}

	machineID, err := w.db.GetMachineID()
	if err != nil {
		return stats, fmt.Errorf("get machine ID: %w", err)
	}

	// Push jobs - need to resolve repo/commit IDs first, then batch insert
	jobs, err := w.db.GetJobsToSync(machineID, syncBatchSize)
	if err != nil {
		return stats, fmt.Errorf("get jobs to sync: %w", err)
	}

	if len(jobs) > 0 {
		// Resolve PostgreSQL repo and commit IDs for each job
		var preparedJobs []JobWithPgIDs
		for _, j := range jobs {
			if j.RepoIdentity == "" {
				log.Printf("Sync: job %s has no repo identity, skipping", j.UUID)
				continue
			}

			pgRepoID, err := pool.GetOrCreateRepo(ctx, j.RepoIdentity)
			if err != nil {
				log.Printf("Sync: failed to get/create repo for job %s: %v", j.UUID, err)
				continue
			}

			var pgCommitID *int64
			if j.CommitSHA != "" {
				id, err := pool.GetOrCreateCommit(ctx, pgRepoID, j.CommitSHA, j.CommitAuthor, j.CommitSubject, j.CommitTimestamp)
				if err != nil {
					log.Printf("Sync: failed to get/create commit for job %s: %v", j.UUID, err)
					continue
				}
				pgCommitID = &id
			}

			preparedJobs = append(preparedJobs, JobWithPgIDs{
				Job:        j,
				PgRepoID:   pgRepoID,
				PgCommitID: pgCommitID,
			})
		}

		// Batch insert jobs
		if len(preparedJobs) > 0 {
			success, err := pool.BatchUpsertJobs(ctx, preparedJobs)
			if err != nil {
				log.Printf("Sync: batch upsert jobs error: %v", err)
			}

			// Only mark successfully synced jobs. Carry each job's pushed
			// snapshot so MarkJobsSynced skips rows changed since (a token-usage
			// capture that lands after the job went terminal, or an attempt reset
			// in the same second).
			var syncedMarks []JobSyncMark
			for i, ok := range success {
				if ok {
					syncedMarks = append(syncedMarks, NewJobSyncMark(preparedJobs[i].Job))
					stats.Jobs++
				}
			}
			if len(syncedMarks) > 0 {
				if err := w.db.MarkJobsSynced(syncedMarks); err != nil {
					log.Printf("Sync: failed to mark jobs synced: %v", err)
				}
			}
		}
	}

	// Push reviews - batch operation
	reviews, err := w.db.GetReviewsToSync(machineID, syncBatchSize)
	if err != nil {
		return stats, fmt.Errorf("get reviews to sync: %w", err)
	}

	if len(reviews) > 0 {
		success, err := pool.BatchUpsertReviews(ctx, reviews)
		if err != nil {
			log.Printf("Sync: batch upsert reviews error: %v", err)
		}

		// Only mark successfully synced reviews
		var syncedReviewIDs []int64
		for i, ok := range success {
			if ok {
				syncedReviewIDs = append(syncedReviewIDs, reviews[i].ID)
				stats.Reviews++
			}
		}
		if len(syncedReviewIDs) > 0 {
			if err := w.db.MarkReviewsSynced(syncedReviewIDs); err != nil {
				log.Printf("Sync: failed to mark reviews synced: %v", err)
			}
		}
	}

	// Push comments - batch operation
	responses, err := w.db.GetCommentsToSync(machineID, syncBatchSize)
	if err != nil {
		return stats, fmt.Errorf("get comments to sync: %w", err)
	}

	if len(responses) > 0 {
		success, err := pool.BatchInsertResponses(ctx, responses)
		if err != nil {
			log.Printf("Sync: batch insert responses error: %v", err)
		}

		// Only mark successfully synced responses
		var syncedResponseIDs []int64
		for i, ok := range success {
			if ok {
				syncedResponseIDs = append(syncedResponseIDs, responses[i].ID)
				stats.Responses++
			}
		}
		if len(syncedResponseIDs) > 0 {
			if err := w.db.MarkCommentsSynced(syncedResponseIDs); err != nil {
				log.Printf("Sync: failed to mark comments synced: %v", err)
			}
		}
	}

	return stats, nil
}

// pullChanges pulls remote changes from PostgreSQL
func (w *SyncWorker) pullChanges(ctx context.Context, pool *PgPool) error {
	_, err := w.pullChangesWithStats(ctx, pool)
	return err
}

// pullChangesWithStats pulls remote changes and returns statistics
func (w *SyncWorker) pullChangesWithStats(ctx context.Context, pool *PgPool) (pushPullStats, error) {
	stats := pushPullStats{}

	machineID, err := w.db.GetMachineID()
	if err != nil {
		return stats, fmt.Errorf("get machine ID: %w", err)
	}

	// Pull jobs
	jobCursor, err := w.db.GetSyncState(SyncStateLastJobCursor)
	if err != nil {
		return stats, fmt.Errorf("get job cursor: %w", err)
	}
	jobQueryCursor := rewindTimestampIDCursor(jobCursor, syncCursorLookback())
	maxJobCursor := jobCursor

	for {
		jobs, newCursor, err := pool.PullJobs(ctx, machineID, jobQueryCursor, 100)
		if err != nil {
			return stats, fmt.Errorf("pull jobs: %w", err)
		}
		if len(jobs) == 0 {
			break
		}

		for _, j := range jobs {
			if err := w.pullJob(j); err != nil {
				// Don't advance cursor if any upsert fails - we'll retry next sync
				return stats, fmt.Errorf("pull job %s: %w", j.UUID, err)
			}
			stats.Jobs++
		}

		jobQueryCursor = newCursor
		maxJobCursor = maxTimestampIDCursor(maxJobCursor, newCursor)
		if err := w.db.SetSyncState(SyncStateLastJobCursor, maxJobCursor); err != nil {
			return stats, fmt.Errorf("save job cursor: %w", err)
		}

		if len(jobs) < 100 {
			break
		}
	}

	// Pull reviews - only for jobs we have locally.
	// Note: knownJobUUIDs is fetched AFTER pulling all jobs above, so it includes
	// any jobs we just pulled in this sync cycle.
	reviewCursor, err := w.db.GetSyncState(SyncStateLastReviewCursor)
	if err != nil {
		return stats, fmt.Errorf("get review cursor: %w", err)
	}
	reviewQueryCursor := rewindTimestampIDCursor(reviewCursor, syncCursorLookback())
	maxReviewCursor := reviewCursor

	knownJobUUIDs, err := w.db.GetKnownJobUUIDs()
	if err != nil {
		return stats, fmt.Errorf("get known job UUIDs: %w", err)
	}

	for {
		reviews, newCursor, err := pool.PullReviews(ctx, machineID, knownJobUUIDs, reviewQueryCursor, 100)
		if err != nil {
			return stats, fmt.Errorf("pull reviews: %w", err)
		}
		if len(reviews) == 0 {
			break
		}

		for _, r := range reviews {
			pr := PulledReview{
				UUID:               r.UUID,
				JobUUID:            r.JobUUID,
				Agent:              r.Agent,
				Prompt:             r.Prompt,
				Output:             r.Output,
				Closed:             r.Closed,
				UpdatedByMachineID: r.UpdatedByMachineID,
				CreatedAt:          r.CreatedAt,
				UpdatedAt:          r.UpdatedAt,
			}
			if err := w.db.UpsertPulledReview(pr); err != nil {
				// Don't advance cursor if any upsert fails - we'll retry next sync
				return stats, fmt.Errorf("pull review %s: %w", r.UUID, err)
			}
			stats.Reviews++
		}

		reviewQueryCursor = newCursor
		maxReviewCursor = maxTimestampIDCursor(maxReviewCursor, newCursor)
		if err := w.db.SetSyncState(SyncStateLastReviewCursor, maxReviewCursor); err != nil {
			return stats, fmt.Errorf("save review cursor: %w", err)
		}

		if len(reviews) < 100 {
			break
		}
	}

	// Pull responses
	responseIDStr, err := w.db.GetSyncState(SyncStateLastResponseID)
	if err != nil {
		return stats, fmt.Errorf("get response cursor: %w", err)
	}
	responseQueryCursor := rewindResponseCursor(responseIDStr, syncCursorLookback())
	maxResponseCursor := responseCursorForMax(responseIDStr)

	for {
		responses, newCursor, err := pool.PullResponses(ctx, machineID, responseQueryCursor, 100)
		if err != nil {
			return stats, fmt.Errorf("pull responses: %w", err)
		}
		if len(responses) == 0 {
			break
		}

		for _, r := range responses {
			pr := PulledResponse{
				UUID:            r.UUID,
				JobUUID:         r.JobUUID,
				Responder:       r.Responder,
				Response:        r.Response,
				SourceMachineID: r.SourceMachineID,
				CreatedAt:       r.CreatedAt,
			}
			if err := w.db.UpsertPulledResponse(pr); err != nil {
				// Don't advance cursor if any upsert fails - we'll retry next sync
				return stats, fmt.Errorf("pull response %s: %w", r.UUID, err)
			}
			stats.Responses++
		}

		responseQueryCursor = newCursor
		maxResponseCursor = maxTimestampIDCursor(maxResponseCursor, newCursor)
		if err := w.db.SetSyncState(SyncStateLastResponseID, maxResponseCursor); err != nil {
			return stats, fmt.Errorf("save response cursor: %w", err)
		}

		if len(responses) < 100 {
			break
		}
	}

	if stats.Jobs > 0 || stats.Reviews > 0 || stats.Responses > 0 {
		log.Printf("Sync: pulled %d jobs, %d reviews, %d responses", stats.Jobs, stats.Reviews, stats.Responses)
	}

	return stats, nil
}

func rewindTimestampIDCursor(cursor string, lookback time.Duration) string {
	if cursor == "" || lookback <= 0 {
		return cursor
	}
	cursorTime, cursorID, ok := parseTimestampIDCursor(cursor)
	if !ok {
		return cursor
	}
	return formatTimestampIDCursor(cursorTime.Add(-lookback), cursorID)
}

func syncCursorLookback() time.Duration {
	raw := os.Getenv(syncCursorLookbackEnv)
	if raw == "" {
		return defaultSyncCursorLookback
	}
	lookback, err := time.ParseDuration(raw)
	if err != nil || lookback < 0 {
		return defaultSyncCursorLookback
	}
	return lookback
}

func rewindResponseCursor(cursor string, lookback time.Duration) string {
	if cursor == "" {
		return ""
	}
	if _, _, ok := parseTimestampIDCursor(cursor); !ok {
		// Older versions stored response cursors as an ID only. Re-read from
		// the beginning once so lower-ID late commits are not stranded.
		return ""
	}
	return rewindTimestampIDCursor(cursor, lookback)
}

func responseCursorForMax(cursor string) string {
	if _, _, ok := parseTimestampIDCursor(cursor); ok {
		return cursor
	}
	return ""
}

func maxTimestampIDCursor(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	aTime, aID, aOK := parseTimestampIDCursor(a)
	bTime, bID, bOK := parseTimestampIDCursor(b)
	if !aOK {
		return b
	}
	if !bOK {
		return a
	}
	if bTime.After(aTime) || (bTime.Equal(aTime) && bID > aID) {
		return b
	}
	return a
}

func parseTimestampIDCursor(cursor string) (time.Time, int64, bool) {
	var ts string
	var id int64
	if _, err := fmt.Sscanf(cursor, "%s %d", &ts, &id); err != nil {
		return time.Time{}, 0, false
	}
	cursorTime, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, 0, false
	}
	return cursorTime, id, true
}

func formatTimestampIDCursor(cursorTime time.Time, cursorID int64) string {
	return fmt.Sprintf("%s %d", cursorTime.Format(time.RFC3339Nano), cursorID)
}

// pullJob inserts a pulled job into SQLite, creating repo/commit as needed
func (w *SyncWorker) pullJob(j PulledJob) error {
	// Get or create repo by identity
	repoID, err := w.db.GetOrCreateRepoByIdentity(j.RepoIdentity)
	if err != nil {
		return fmt.Errorf("get or create repo: %w", err)
	}

	// Get or create commit if we have one
	var commitID *int64
	if j.CommitSHA != "" {
		id, err := w.db.GetOrCreateCommitByRepoAndSHA(repoID, j.CommitSHA, j.CommitAuthor, j.CommitSubject, j.CommitTimestamp)
		if err != nil {
			return fmt.Errorf("get or create commit: %w", err)
		}
		commitID = &id
	}

	return w.db.UpsertPulledJob(j, repoID, commitID)
}

// HealthCheck returns the health status of the sync worker
func (w *SyncWorker) HealthCheck() (healthy bool, message string) {
	w.mu.Lock()
	running := w.running
	pool := w.pgPool
	w.mu.Unlock()

	if !running {
		return false, "not running"
	}

	if pool == nil {
		return false, "not connected"
	}

	// Check if we can ping PostgreSQL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := pool.Ping(ctx); err != nil {
		return false, "connection lost"
	}

	return true, "connected"
}
