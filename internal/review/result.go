// Package review provides daemon-free review orchestration: parallel
// batch execution, synthesis, and comment formatting.
package review

import (
	"strings"
	"unicode/utf8"
)

// ReviewResult holds the outcome of a single review in a batch.
// Decoupled from storage.BatchReviewResult for daemon-free use.
type ReviewResult struct {
	Agent      string
	ReviewType string
	Output     string
	Status     string // ResultDone, ResultFailed, or ResultSkipped
	Error      string

	// Skipped/SkipReason are populated for skipped (auto-design) rows so
	// synthesis can render them as a distinct short section instead of
	// crashing on missing Output.
	Skipped    bool
	SkipReason string
}

// Result status values for ReviewResult.Status.
const (
	ResultDone    = "done"
	ResultFailed  = "failed"
	ResultSkipped = "skipped"
)

// MaxCommentLen is the maximum length for a GitHub PR comment.
// GitHub's hard limit is ~65536; we leave headroom.
const MaxCommentLen = 60000

// TrimPartialRune removes a trailing incomplete UTF-8 sequence that
// may result from slicing a string at an arbitrary byte offset. Only
// the last rune is inspected — pre-existing invalid bytes elsewhere
// in the string are left untouched.
func TrimPartialRune(s string) string {
	if len(s) == 0 {
		return s
	}
	r, size := utf8.DecodeLastRuneInString(s)
	if r == utf8.RuneError && size == 1 {
		// Walk back past continuation bytes of the broken
		// sequence (at most 3 bytes for a 4-byte rune).
		i := len(s) - 1
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		// i now points at a rune-start byte. If it decodes to a
		// valid rune, the trailing bytes are orphan continuation
		// bytes — trim only those, keeping the valid rune.
		if r2, sz := utf8.DecodeRuneInString(s[i:]); r2 != utf8.RuneError || sz > 1 {
			return s[:i+sz]
		}
		// The rune-start byte itself is part of the broken
		// sequence (e.g., a multi-byte lead with too few
		// continuation bytes). Trim from i.
		return s[:i]
	}
	return s
}

// QuotaErrorPrefix is prepended to error messages when a review
// fails due to agent quota exhaustion rather than a real error.
// Matches the prefix set by internal/daemon/worker.go.
const QuotaErrorPrefix = "quota: "

// OutageErrorPrefix is prepended to error messages when a review failed due to
// a transient provider outage (429 / stream-disconnect / 5xx), so the batch
// layer can treat it as retryable rather than a genuine failure.
const OutageErrorPrefix = "outage: "

// OutageError prepends OutageErrorPrefix unless already present.
func OutageError(msg string) string {
	if strings.HasPrefix(msg, OutageErrorPrefix) {
		return msg
	}
	return OutageErrorPrefix + msg
}

// TimeoutErrorPrefix is prepended to error messages when a batch job
// is canceled because the batch exceeded its timeout and results were
// posted with the available reviews.
const TimeoutErrorPrefix = "timeout: "
