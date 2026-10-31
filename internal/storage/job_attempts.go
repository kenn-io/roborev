package storage

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// The daemon owns one DB. Pulls and reruns take the write lock; worker
// transitions hold the read lock through persistence and completion effects.
// Running agents hold no lock, so a newer pull can cancel them immediately.
type jobAttempts struct {
	mu     attemptLock
	active map[int64]*activeJobAttempt
}

// attemptLock is a reader/writer lock whose writers can stop waiting when
// their context ends. A new attempt registers for cancellation and timeout
// only after it takes the write lock, so with sync.RWMutex a slow transition
// would leave every attempt claimed behind it with no timeout at all. As with
// sync.RWMutex, a waiting writer holds back new readers. The zero value is
// unlocked.
type attemptLock struct {
	mu             sync.Mutex
	readers        int
	writer         bool
	waitingWriters int
	// changed is closed, then replaced, whenever a waiter may be able to
	// proceed.
	changed chan struct{}
}

func (l *attemptLock) waitLocked() <-chan struct{} {
	if l.changed == nil {
		l.changed = make(chan struct{})
	}
	return l.changed
}

func (l *attemptLock) wakeLocked() {
	if l.changed != nil {
		close(l.changed)
		l.changed = nil
	}
}

func (l *attemptLock) RLock() {
	l.mu.Lock()
	for l.writer || l.waitingWriters > 0 {
		wait := l.waitLocked()
		l.mu.Unlock()
		<-wait
		l.mu.Lock()
	}
	l.readers++
	l.mu.Unlock()
}

func (l *attemptLock) RUnlock() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.readers--
	if l.readers == 0 {
		l.wakeLocked()
	}
}

func (l *attemptLock) Lock() {
	_ = l.LockContext(context.Background())
}

// LockContext takes the write lock, or returns ctx.Err() if ctx ends first.
func (l *attemptLock) LockContext(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waitingWriters++
	for l.writer || l.readers > 0 {
		wait := l.waitLocked()
		l.mu.Unlock()
		select {
		case <-wait:
			l.mu.Lock()
		case <-ctx.Done():
			l.mu.Lock()
			l.waitingWriters--
			// Readers held back by this writer may proceed now.
			l.wakeLocked()
			return ctx.Err()
		}
	}
	l.waitingWriters--
	l.writer = true
	return nil
}

func (l *attemptLock) Unlock() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writer = false
	l.wakeLocked()
}

type activeJobAttempt struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// StartJobAttempt registers cancellation for a claimed execution. The returned
// release must run after the worker has closed its output and cancellation
// registration, so a rerun cannot reuse them while the old process unwinds.
// A retry may already be claimed; it waits for that cleanup before starting.
func (db *DB) StartJobAttempt(ctx context.Context, job *ReviewJob, workerID string, cancel context.CancelFunc) (func(), error) {
	for {
		if err := db.attempts.mu.LockContext(ctx); err != nil {
			return nil, err
		}
		if previous := db.attempts.active[job.ID]; previous != nil {
			db.attempts.mu.Unlock()
			select {
			case <-previous.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		status, err := db.jobAttemptStatus(job, workerID)
		if err != nil || status == "" {
			db.attempts.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, sql.ErrNoRows
		}
		if status == JobStatusCanceled {
			cancel()
		}
		attempt := &activeJobAttempt{cancel: cancel, done: make(chan struct{})}
		if db.attempts.active == nil {
			db.attempts.active = make(map[int64]*activeJobAttempt)
		}
		db.attempts.active[job.ID] = attempt
		db.attempts.mu.Unlock()
		return func() {
			db.attempts.mu.Lock()
			defer db.attempts.mu.Unlock()
			delete(db.attempts.active, job.ID)
			close(attempt.done)
		}, nil
	}
}

func (db *DB) jobAttemptStatus(job *ReviewJob, workerID string) (JobStatus, error) {
	var enqueuedAt string
	var status JobStatus
	owner := ""
	if job.SourceMachineID != nil {
		owner = job.SourceMachineID.String()
	}
	err := db.QueryRow(`SELECT enqueued_at, status FROM review_jobs WHERE id = ? AND status IN ('running', 'canceled') AND worker_id = ? AND COALESCE(source_machine_id, '') = ? COLLATE NOCASE AND COALESCE(started_at, '') = ?`, job.ID, workerID, owner, job.StartedAtRaw).Scan(&enqueuedAt, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !parseSQLiteTime(enqueuedAt).Equal(job.EnqueuedAt) {
		return "", nil
	}
	return status, nil
}

// RunJobAttempt runs a worker transition only while its claim is current.
// Sync cannot replace that attempt until transition and its side effects return.
// The callback must not pull jobs, rerun jobs, or call RunJobAttempt recursively.
func (db *DB) RunJobAttempt(job *ReviewJob, workerID string, transition func()) (bool, error) {
	db.attempts.mu.RLock()
	defer db.attempts.mu.RUnlock()
	status, err := db.jobAttemptStatus(job, workerID)
	if err != nil || status == "" {
		return false, err
	}
	transition()
	return true, nil
}

// SaveJobPromptForAttempt prevents a prompt built by a displaced worker from
// replacing the sole prompt belonging to the newly pulled attempt.
func (db *DB) SaveJobPromptForAttempt(job *ReviewJob, workerID, prompt string) (bool, error) {
	var saveErr error
	current, err := db.RunJobAttempt(job, workerID, func() {
		raw, encodeErr := encodeSessionProvenance(job.SessionProvenance)
		if encodeErr != nil {
			saveErr = encodeErr
			return
		}
		tx, beginErr := db.Begin()
		if beginErr != nil {
			saveErr = beginErr
			return
		}
		defer func() { _ = tx.Rollback() }()
		if saveErr = setJobContent(context.Background(), tx, job.ID, jobContentPrompt, prompt); saveErr != nil {
			return
		}
		if saveErr = setJobContent(context.Background(), tx, job.ID, jobContentProvenance, raw); saveErr != nil {
			return
		}
		if _, saveErr = tx.Exec(`UPDATE review_jobs SET synced_at=NULL,updated_at=? WHERE id=?`, preciseTimestampAt(time.Now()), job.ID); saveErr != nil {
			return
		}
		saveErr = tx.Commit()
	})
	return current, errors.Join(err, saveErr)
}
