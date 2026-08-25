// Package review provides daemon-free review orchestration: parallel
// batch execution, synthesis, and comment formatting.
package review

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/roborev/internal/storage"
)

// ReviewResult holds the outcome of a single review in a batch.
// Decoupled from storage.BatchReviewResult for daemon-free use.
type ReviewResult struct {
	Agent      string
	ReviewType string
	Output     string
	Status     string // ResultDone, ResultFailed, or ResultSkipped
	Error      string
	// Verdict is the canonical pass/fail result. The runner derives prose
	// verdicts once and takes custom-review verdicts from structured output.
	// The empty value is reserved for historical or manually assembled results.
	Verdict storage.Verdict
	// Structured retains schema output so later severity thresholds can be
	// applied without reparsing or asking a synthesis agent to infer findings
	// from rendered Markdown.
	Structured *StructuredReview
	// StructuredOutput is the unfiltered JSON returned by the agent. Queued
	// reviews persist it so a later panel threshold can use the same findings.
	StructuredOutput json.RawMessage
	// StructuredMinSeverity is the threshold already applied to Output and
	// Verdict. Later consumers combine it with their own threshold instead of
	// accidentally restoring findings that were already excluded.
	StructuredMinSeverity string

	// Skipped/SkipReason are populated for skipped (auto-design) rows so
	// synthesis can render them as a distinct short section instead of
	// crashing on missing Output.
	Skipped    bool
	SkipReason string

	// AllowFailure means this panel member is allowed to fail without making an
	// otherwise successful panel fail. It is set from the resolved member config
	// stored with the job, not from live config.
	AllowFailure bool
}

// Passed returns the canonical review verdict when one is available, falling
// back to Markdown parsing for prose reviews.
func (r ReviewResult) Passed() bool {
	if r.Verdict != storage.VerdictUnknown {
		return r.Verdict.Passed()
	}
	return storage.ParseVerdict(r.Output) == storage.VerdictPass
}

// FilterStructured applies minSeverity to schema-backed output and updates the
// rendered text and verdict together. Prose review results are unchanged.
func (r ReviewResult) FilterStructured(minSeverity string) ReviewResult {
	if r.Structured == nil {
		return r
	}
	effectiveMinSeverity := stricterMinSeverity(
		r.StructuredMinSeverity, minSeverity,
	)
	filtered := r.Structured.Filter(effectiveMinSeverity)
	r.Structured = &filtered
	r.StructuredMinSeverity = effectiveMinSeverity
	r.Verdict = storage.VerdictFromPassed(filtered.Passed())
	r.Output = filtered.Markdown()
	return r
}

// Result status values for ReviewResult.Status.
const (
	ResultDone    = "done"
	ResultFailed  = "failed"
	ResultSkipped = "skipped"
)

const noReviewOutputPlaceholder = "No review output generated"

// HasSubstantiveOutput reports whether any completed review produced
// agent-authored output. Failed and skipped results never qualify, even when
// they carry diagnostic text, and neither does the placeholder returned by
// adapters when an agent completes without output.
func HasSubstantiveOutput(results []ReviewResult) bool {
	return slices.ContainsFunc(results, IsSubstantiveOutput)
}

// IsSubstantiveOutput reports whether one completed review produced
// agent-authored output.
func IsSubstantiveOutput(result ReviewResult) bool {
	output := strings.TrimSpace(result.Output)
	return result.Status == ResultDone &&
		output != "" && output != noReviewOutputPlaceholder
}

// MaxCommentLen is the maximum length for a GitHub PR comment.
// GitHub's hard limit is ~65536; we leave headroom.
const MaxCommentLen = 60000

// CommentTruncSuffix is appended to a forge comment body when it had to
// be cut to fit MaxCommentLen.
const CommentTruncSuffix = "\n\n...(truncated — comment exceeded size limit)"

// TruncateComment caps a forge comment body at MaxCommentLen, replacing
// the overflow with CommentTruncSuffix and keeping the cut UTF-8 safe.
func TruncateComment(body string) string {
	if len(body) <= MaxCommentLen {
		return body
	}
	return TrimPartialRune(body[:MaxCommentLen-len(CommentTruncSuffix)]) + CommentTruncSuffix
}

// OutputTruncSuffix marks a single review's output as cut short. It is
// deliberately not CommentTruncSuffix: this is one section being capped inside a
// larger body, not the comment as a whole hitting the forge's size limit.
const OutputTruncSuffix = "\n\n...(truncated)"

// TruncateOutput caps one review's output at limit, appending OutputTruncSuffix
// and keeping the cut UTF-8 safe. limit is the budget for the output itself, so
// the suffix sits outside it — unlike TruncateComment, where the suffix has to
// fit inside a hard cap.
func TruncateOutput(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return TrimPartialRune(s[:limit]) + OutputTruncSuffix
}

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

// UnavailableErrorPrefix is prepended when an agent fails before producing
// valid protocol output and no existing quota, session, or transient category
// applies.
const UnavailableErrorPrefix = "unavailable: "

// UnavailableError prepends UnavailableErrorPrefix unless already present.
func UnavailableError(msg string) string {
	if strings.HasPrefix(msg, UnavailableErrorPrefix) {
		return msg
	}
	return UnavailableErrorPrefix + msg
}

// TimeoutErrorPrefix is prepended to error messages when a batch job
// is canceled because the batch exceeded its timeout and results were
// posted with the available reviews.
const TimeoutErrorPrefix = "timeout: "
