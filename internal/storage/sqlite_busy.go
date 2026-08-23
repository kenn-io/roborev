package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// SQLite result codes for lock contention. Named locally so we do not
// depend on modernc.org/sqlite/lib constants.
const (
	sqliteBusy   = 5 // SQLITE_BUSY
	sqliteLocked = 6 // SQLITE_LOCKED
)

var errSQLiteBusyAttemptTimeout = errors.New("sqlite busy attempt timed out")

// IsSQLiteBusy reports whether err is lock contention (SQLITE_BUSY /
// SQLITE_LOCKED), including the "database is locked (5) (SQLITE_BUSY)"
// text modernc.org/sqlite emits after busy_timeout elapses.
func IsSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errSQLiteBusyAttemptTimeout) {
		return true
	}
	if se, ok := errors.AsType[*sqlite.Error](err); ok {
		switch se.Code() {
		case sqliteBusy, sqliteLocked:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked")
}

func retryOnSQLiteBusy[T any](
	ctx context.Context,
	attempts int,
	attemptTimeout time.Duration,
	backoff time.Duration,
	sleep func(context.Context, time.Duration) error,
	fn func(context.Context) (T, error),
) (T, error) {
	var zero T
	if attempts < 1 {
		attempts = 1
	}
	if sleep == nil {
		sleep = waitForSQLiteBusyRetry
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		attemptCtx := ctx
		cancel := func() {}
		if attemptTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, attemptTimeout)
		}
		v, err := fn(attemptCtx)
		attemptTimedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if err == nil {
			return v, nil
		}
		if attemptTimedOut && !IsSQLiteBusy(err) {
			err = errors.Join(errSQLiteBusyAttemptTimeout, err)
		}
		if !IsSQLiteBusy(err) && !attemptTimedOut {
			return v, err
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		lastErr = err
		if i+1 < attempts {
			if err := sleep(ctx, backoff<<i); err != nil {
				return zero, err
			}
		}
	}
	return zero, lastErr
}

func waitForSQLiteBusyRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
