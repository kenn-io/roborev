package daemon

import (
	"strings"

	reviewpkg "go.kenn.io/roborev/internal/review"
)

// OutcomeKind enumerates how a finalized CI panel run should be resolved against
// its member results and the HEAD's retry state. It is the decision the
// finalize path branches on: post results, defer for a retry, give up with a
// non-blocking note, or post the all-skipped summary.
type OutcomeKind int

const (
	// OutcomePost posts the combined/synthesis review comment: at least one
	// member produced real review output.
	OutcomePost OutcomeKind = iota
	// OutcomeDeferTransient defers the run for a later retry: no member
	// succeeded and at least one failed on a transient provider outage. The
	// finalize path posts nothing unless the transient retry wall is exhausted.
	OutcomeDeferTransient
	// OutcomeDeferGenuine defers the run for a later retry: no member succeeded,
	// none failed transiently, and at least one failed genuinely, but the
	// consecutive-genuine streak has not yet hit the give-up cap.
	OutcomeDeferGenuine
	// OutcomeGenuineGiveUp posts a genuine-failure soft note with a blocking
	// commit status: a genuine failure recurred up to the give-up cap.
	OutcomeGenuineGiveUp
	// OutcomeAllSkip posts the all-skipped summary: every member was a
	// quota/timeout skip (or the member set was empty), with no real result.
	OutcomeAllSkip
)

// PanelOutcome is the result of classifyPanelOutcome: the resolved OutcomeKind
// plus a representative error excerpt for the comment/defer record. The excerpt
// is the first transient error for transient outcomes and the first genuine
// error for genuine outcomes; it is empty for post/all-skip.
type PanelOutcome struct {
	Kind             OutcomeKind
	LastErrorExcerpt string
}

// classifyPanelOutcome decides a panel run's finalize outcome from its member
// results, the synthesis job's outcome, and the HEAD's consecutive-genuine
// streak. It is PURE: no DB, no network, so it unit-tests fast.
// consecutiveGenuine is the streak recorded BEFORE this attempt (0 when no
// attempt row exists yet). synthesis is the synthesis job's outcome as a
// ReviewResult, or nil when it is not in a failed state (done/missing).
//
// Precedence, applied in order:
//  0. synthesis failed on quota exhaustion or a transient provider outage ->
//     OutcomeDeferTransient. The consolidation step could not run, not that the
//     reviews are unusable, so wait for the real synthesis (when quota resets or
//     the provider recovers) rather than post the degraded "Synthesis
//     unavailable" raw fallback. A genuine synthesis failure is NOT caught here;
//     it falls through to the member rules below, where a member with output
//     still posts the raw fallback (retrying a deterministic error would not
//     help).
//  1. any done member with non-empty output -> OutcomePost (a partial review is
//     better than nothing once any reviewer landed real output).
//  2. else any transient-outage failure -> OutcomeDeferTransient (wait for the
//     real review rather than post a failed/partial result).
//  3. else any genuine failure -> OutcomeGenuineGiveUp when this attempt would
//     hit the give-up cap (consecutiveGenuine+1), otherwise OutcomeDeferGenuine.
//  4. else (all quota/timeout skips, or empty) -> OutcomeAllSkip.
func classifyPanelOutcome(
	results []reviewpkg.ReviewResult, synthesis *reviewpkg.ReviewResult, consecutiveGenuine int,
) PanelOutcome {
	if synthesis != nil &&
		(reviewpkg.IsQuotaFailure(*synthesis) || reviewpkg.IsTransientFailure(*synthesis)) {
		return PanelOutcome{Kind: OutcomeDeferTransient, LastErrorExcerpt: synthesis.Error}
	}
	if hasReviewOutput(results) {
		return PanelOutcome{Kind: OutcomePost}
	}
	if r := firstMatch(results, reviewpkg.IsTransientFailure); r != nil {
		return PanelOutcome{Kind: OutcomeDeferTransient, LastErrorExcerpt: r.Error}
	}
	if r := firstMatch(results, reviewpkg.IsGenuineFailure); r != nil {
		kind := OutcomeDeferGenuine
		if reviewpkg.DefaultRetrySchedule.GenuineExhausted(consecutiveGenuine + 1) {
			kind = OutcomeGenuineGiveUp
		}
		return PanelOutcome{Kind: kind, LastErrorExcerpt: r.Error}
	}
	return PanelOutcome{Kind: OutcomeAllSkip}
}

// hasReviewOutput reports whether any member produced real review output (rule 1
// of classifyPanelOutcome): a done status with non-empty output.
func hasReviewOutput(results []reviewpkg.ReviewResult) bool {
	for _, r := range results {
		if r.Status == reviewpkg.ResultDone && strings.TrimSpace(r.Output) != "" {
			return true
		}
	}
	return false
}

// firstMatch returns a pointer to the first result satisfying pred, or nil when
// none match. The returned pointer is to a copy, safe to read after the loop.
func firstMatch(results []reviewpkg.ReviewResult, pred func(reviewpkg.ReviewResult) bool) *reviewpkg.ReviewResult {
	for _, r := range results {
		if pred(r) {
			return &r
		}
	}
	return nil
}
