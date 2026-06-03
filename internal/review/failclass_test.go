package review

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsTransientFailure(t *testing.T) {
	assert := assert.New(t)
	transient := ReviewResult{Status: ResultFailed, Error: OutageErrorPrefix + "429 too many requests"}
	quota := ReviewResult{Status: ResultFailed, Error: QuotaErrorPrefix + "quota exceeded"}
	genuine := ReviewResult{Status: ResultFailed, Error: "model not supported"}
	timeout := ReviewResult{Status: "canceled", Error: TimeoutErrorPrefix + "posted early"}
	success := ReviewResult{Status: ResultDone, Output: "looks good"}

	assert.True(IsTransientFailure(transient))
	assert.False(IsTransientFailure(quota))
	assert.False(IsTransientFailure(genuine))
	assert.False(IsTransientFailure(timeout))
	assert.False(IsTransientFailure(success))

	transient2 := ReviewResult{Status: ResultFailed, Error: OutageErrorPrefix + "503 service unavailable"}
	assert.Equal(1, CountTransientFailures([]ReviewResult{transient, quota, genuine, timeout, success}))
	assert.Equal(2, CountTransientFailures([]ReviewResult{transient, quota, transient2, genuine, success}))
	assert.Equal(0, CountTransientFailures(nil))
	assert.Equal(0, CountTransientFailures([]ReviewResult{}))

	// IsGenuineFailure: only the deterministic failure qualifies.
	assert.True(IsGenuineFailure(genuine))
	assert.False(IsGenuineFailure(transient))
	assert.False(IsGenuineFailure(quota))
	assert.False(IsGenuineFailure(timeout))
	assert.False(IsGenuineFailure(success))
}
