package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSQLiteBusy(t *testing.T) {
	assert.False(t, IsSQLiteBusy(nil))
	assert.False(t, IsSQLiteBusy(errors.New("disk I/O error")))
	assert.False(t, IsSQLiteBusy(errors.New("no such table: review_jobs")))
	assert.True(t, IsSQLiteBusy(errors.New("database is locked (5) (SQLITE_BUSY)")))
	assert.True(t, IsSQLiteBusy(fmt.Errorf("claim job: %w", errors.New("SQLITE_BUSY"))))
	assert.True(t, IsSQLiteBusy(errors.New("database is locked")))
	assert.True(t, IsSQLiteBusy(errors.New("SQLITE_LOCKED")))
}

func TestIsSQLiteBusyRealError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "busy.db")
	dsn := path + "?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(0)"

	holder, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Close() })
	_, err = holder.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY); INSERT INTO t (id) VALUES (1)`)
	require.NoError(t, err)
	tx, err := holder.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec(`UPDATE t SET id = 1`)
	require.NoError(t, err)

	contender, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = contender.Close() })
	_, err = contender.Exec(`UPDATE t SET id = 2`)
	require.Error(t, err)
	assert.True(t, IsSQLiteBusy(err), "got %v", err)
}

func TestRetryOnSQLiteBusySucceedsAfterContention(t *testing.T) {
	n := 0
	var sleeps []time.Duration
	job, err := retryOnSQLiteBusy(context.Background(), 4, 0, 50*time.Millisecond, func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}, func(context.Context) (*ReviewJob, error) {
		n++
		if n < 3 {
			return nil, errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		return &ReviewJob{ID: 7}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, int64(7), job.ID)
	assert.Equal(t, 3, n)
	assert.Equal(t, []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}, sleeps)
}

func TestRetryOnSQLiteBusyDoesNotRetryPermanentErrors(t *testing.T) {
	n := 0
	sleepCalls := 0
	_, err := retryOnSQLiteBusy(context.Background(), 4, 0, time.Millisecond, func(context.Context, time.Duration) error {
		sleepCalls++
		return nil
	}, func(context.Context) (*ReviewJob, error) {
		n++
		return nil, errors.New("disk I/O error")
	})
	require.EqualError(t, err, "disk I/O error")
	assert.Equal(t, 1, n)
	assert.Equal(t, 0, sleepCalls)
}

func TestRetryOnSQLiteBusyGivesUp(t *testing.T) {
	n := 0
	_, err := retryOnSQLiteBusy(context.Background(), 3, 0, time.Millisecond, func(context.Context, time.Duration) error { return nil }, func(context.Context) (*ReviewJob, error) {
		n++
		return nil, errors.New("database is locked (5) (SQLITE_BUSY)")
	})
	require.Error(t, err)
	assert.True(t, IsSQLiteBusy(err))
	assert.Equal(t, 3, n)
}

func TestRetryOnSQLiteBusyNilIsSuccess(t *testing.T) {
	job, err := retryOnSQLiteBusy(context.Background(), 4, 0, time.Millisecond, nil, func(context.Context) (*ReviewJob, error) {
		return nil, nil
	})
	require.NoError(t, err)
	assert.Nil(t, job)
}

func TestRetryOnSQLiteBusyReturnsSuccessAfterAttemptDeadline(t *testing.T) {
	calls := 0
	job, err := retryOnSQLiteBusy(context.Background(), 4, 5*time.Millisecond, 0, nil, func(ctx context.Context) (*ReviewJob, error) {
		calls++
		<-ctx.Done()
		return &ReviewJob{ID: 7}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, int64(7), job.ID)
	assert.Equal(t, 1, calls)
}

func TestRetryOnSQLiteBusyStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := retryOnSQLiteBusy(ctx, 4, 0, time.Hour, nil, func(context.Context) (*ReviewJob, error) {
		calls++
		cancel()
		return nil, errors.New("database is locked (5) (SQLITE_BUSY)")
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
}

func TestRetryOnSQLiteBusyUsesEachAttemptBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	calls := 0

	_, err := retryOnSQLiteBusy(ctx, 4, 10*time.Millisecond, 0, nil, func(attemptCtx context.Context) (*ReviewJob, error) {
		calls++
		<-attemptCtx.Done()
		return nil, attemptCtx.Err()
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, IsSQLiteBusy(err))
	assert.Equal(t, 4, calls)
}
