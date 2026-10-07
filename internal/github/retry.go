package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v7"
	googlegithub "github.com/google/go-github/v91/github"
	"golang.org/x/net/http2"
)

// readFailure retains typed SDK errors for callers while keeping provider text,
// URLs, and credentials out of daemon diagnostics.
type readFailure struct {
	operation string
	detail    string
	err       error
}

func (e *readFailure) Error() string { return e.operation + ": " + e.detail }
func (e *readFailure) Unwrap() error { return e.err }

// ReadErrorSummary returns safe operation and failure details for GitHub reads.
// Unknown errors have no summary; their text may contain subprocess output.
func ReadErrorSummary(err error) string {
	if failure, ok := errors.AsType[*readFailure](err); ok {
		return failure.Error()
	}
	return ""
}

// readGitHub owns retries for SDK GET calls. Each invocation creates its own
// response decoder and HTTP timeout, so partial bodies never escape an attempt.
func readGitHub[T any](ctx context.Context, operation string, read func() (T, *googlegithub.Response, error)) (T, *googlegithub.Response, error) {
	var response *googlegithub.Response
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = time.Second
	policy.MaxInterval = 8 * time.Second
	result, err := backoff.Retry(ctx, func() (T, error) {
		var zero T
		if err := ctx.Err(); err != nil {
			return zero, backoff.Permanent(err)
		}
		value, resp, err := read()
		response = resp
		if err == nil {
			return value, nil
		}
		failure := &readFailure{operation: operation, detail: readFailureDetail(err, resp), err: err}
		if ctx.Err() != nil {
			return zero, backoff.Permanent(&readFailure{operation: operation, detail: readFailureDetail(ctx.Err(), nil), err: ctx.Err()})
		}
		if !retryableRead(err, resp) {
			return zero, backoff.Permanent(failure)
		}
		if delay, ok := readRetryAfter(err, resp); ok {
			return zero, backoff.RetryAfter(delay, failure)
		}
		return zero, failure
	}, backoff.WithBackOff(policy), backoff.WithMaxTries(4), backoff.WithMaxElapsedTime(2*time.Minute), backoff.WithNotify(func(err error, delay time.Duration) {
		log.Printf("GitHub read retry: %s; retrying in %s", ReadErrorSummary(err), delay.Round(time.Millisecond))
	}))
	if err != nil {
		if ctx.Err() != nil {
			return result, response, &readFailure{operation: operation, detail: readFailureDetail(ctx.Err(), nil), err: ctx.Err()}
		}
		return result, response, backoff.AsRetryError(err).LastErr
	}
	return result, response, nil
}

func retryableRead(err error, resp *googlegithub.Response) bool {
	if isHTTP2StreamInterruption(err) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	if _, ok := errors.AsType[*googlegithub.RateLimitError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*googlegithub.AbuseRateLimitError](err); ok {
		return true
	}
	if resp != nil {
		switch resp.StatusCode {
		case 408, 429, 500, 502, 503, 504:
			return true
		}
	}
	return false
}

func readFailureDetail(err error, resp *googlegithub.Response) string {
	var networkError net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &networkError) && networkError.Timeout():
		return "timeout"
	case isHTTP2StreamInterruption(err), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "network interruption"
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return "network interruption"
	}
	if resp != nil {
		status := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if resp.StatusCode == 429 {
			return "rate limit (" + status + ")"
		}
		if _, ok := errors.AsType[*googlegithub.RateLimitError](err); ok {
			return "rate limit (" + status + ")"
		}
		if _, ok := errors.AsType[*googlegithub.AbuseRateLimitError](err); ok {
			return "rate limit (" + status + ")"
		}
		if resp.StatusCode >= 400 {
			return status
		}
	}
	return "invalid response"
}

func readRetryAfter(err error, resp *googlegithub.Response) (time.Duration, bool) {
	if resp != nil {
		value := resp.Header.Get("Retry-After")
		if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
			return time.Duration(seconds) * time.Second, true
		}
		if at, err := http.ParseTime(value); err == nil {
			return max(time.Until(at), 0), true
		}
	}
	if rate, ok := errors.AsType[*googlegithub.RateLimitError](err); ok {
		return max(time.Until(rate.Rate.Reset.Time), 0), true
	}
	if rate, ok := errors.AsType[*googlegithub.AbuseRateLimitError](err); ok && rate.RetryAfter != nil {
		return *rate.RetryAfter, true
	}
	return 0, false
}

func isHTTP2StreamInterruption(err error) bool {
	if stream, ok := errors.AsType[http2.StreamError](err); ok {
		switch stream.Code {
		case http2.ErrCodeCancel, http2.ErrCodeRefusedStream, http2.ErrCodeInternal:
			return true
		}
	}
	return false
}
