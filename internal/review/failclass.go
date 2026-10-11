package review

import "strings"

// UnsavedOutcomeError identifies a daemon storage failure recovered by the sweep.
const UnsavedOutcomeError = "daemon did not save the job's outcome"

// IsTransientFailure reports whether a review failed due to a transient
// provider outage (tagged with OutageErrorPrefix), as opposed to quota,
// timeout, or a genuine/deterministic failure.
func IsTransientFailure(r ReviewResult) bool {
	return r.Status == ResultFailed && strings.HasPrefix(r.Error, OutageErrorPrefix)
}

// CountTransientFailures returns the number of transient-outage failures.
func CountTransientFailures(reviews []ReviewResult) int {
	n := 0
	for _, r := range reviews {
		if IsTransientFailure(r) {
			n++
		}
	}
	return n
}

// IsGenuineFailure reports a deterministic review failure, excluding daemon recovery.
func IsGenuineFailure(r ReviewResult) bool {
	return r.Status == ResultFailed &&
		r.Error != UnsavedOutcomeError && !IsQuotaFailure(r) && !IsTransientFailure(r) && !IsTimeoutCancellation(r)
}
