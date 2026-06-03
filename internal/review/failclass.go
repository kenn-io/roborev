package review

import "strings"

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

// IsGenuineFailure reports a failed review that is neither quota, timeout, nor
// transient — i.e. a deterministic failure that retrying will not fix soon.
func IsGenuineFailure(r ReviewResult) bool {
	return r.Status == ResultFailed &&
		!IsQuotaFailure(r) && !IsTransientFailure(r) && !IsTimeoutCancellation(r)
}
