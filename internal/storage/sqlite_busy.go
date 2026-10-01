package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
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
	initialInterval time.Duration,
	notify backoff.Notify,
	fn func(context.Context) (T, error),
) (T, error) {
	var zero T
	attempts = max(attempts, 1)
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = initialInterval
	policy.MaxInterval = time.Duration(1<<63 - 1)
	policy.Multiplier = 2
	v, err := backoff.Retry(ctx, func() (T, error) {
		if err := ctx.Err(); err != nil {
			return zero, backoff.Permanent(err)
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
			return v, backoff.Permanent(err)
		}
		if err := ctx.Err(); err != nil {
			return zero, backoff.Permanent(err)
		}
		return zero, err
	}, backoff.WithBackOff(policy), backoff.WithMaxTries(uint(attempts)), backoff.WithMaxElapsedTime(0),
		backoff.WithNotify(notify))
	if err == nil {
		return v, nil
	}
	retryErr := backoff.AsRetryError(err)
	if errors.Is(retryErr.Cause, backoff.ErrPermanent) {
		return v, retryErr.LastErr
	}
	if !errors.Is(retryErr.Cause, backoff.ErrExhausted) {
		return zero, ctx.Err()
	}
	return zero, retryErr.LastErr
}
