package storage

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// The daemon owns one DB. Pulls and reruns take the write lock; worker
// transitions hold the read lock through persistence and completion effects.
// Running agents hold no lock, so a newer pull can cancel them immediately.
type jobAttempts struct {
	mu     sync.RWMutex
	active map[int64]*activeJobAttempt
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
		db.attempts.mu.Lock()
		if previous := db.attempts.active[job.ID]; previous != nil {
			db.attempts.mu.Unlock()
			select {
			case <-previous.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		current, err := db.isCurrentJobAttempt(job, workerID)
		if err != nil || !current {
			db.attempts.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, sql.ErrNoRows
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

func (db *DB) isCurrentJobAttempt(job *ReviewJob, workerID string) (bool, error) {
	var enqueuedAt string
	err := db.QueryRow(`SELECT enqueued_at FROM review_jobs WHERE id = ? AND status IN ('running', 'canceled') AND worker_id = ?`, job.ID, workerID).Scan(&enqueuedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && parseSQLiteTime(enqueuedAt).Equal(job.EnqueuedAt), err
}

// RunJobAttempt runs a worker transition only while its claim is current.
// Sync cannot replace that attempt until transition and its side effects return.
// The callback must not pull jobs, rerun jobs, or call RunJobAttempt recursively.
func (db *DB) RunJobAttempt(job *ReviewJob, workerID string, transition func()) (bool, error) {
	db.attempts.mu.RLock()
	defer db.attempts.mu.RUnlock()
	current, err := db.isCurrentJobAttempt(job, workerID)
	if err != nil || !current {
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
		saveErr = db.SaveJobPrompt(job.ID, prompt)
	})
	return current, errors.Join(err, saveErr)
}
